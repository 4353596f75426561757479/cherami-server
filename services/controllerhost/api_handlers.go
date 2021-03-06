// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package controllerhost

import (
	"time"

	"github.com/pborman/uuid"
	"github.com/uber-common/bark"
	"github.com/uber/cherami-server/common"
	"github.com/uber/cherami-server/common/metrics"
	m "github.com/uber/cherami-thrift/.generated/go/metadata"
	"github.com/uber/cherami-thrift/.generated/go/shared"
	"github.com/uber/tchannel-go/thrift"
)

type dstType int

const (
	dstTypeDLQ   = dstType(-2) // metadata doesn't have this type
	dstTypePlain = dstType(shared.DestinationType_PLAIN)
	dstTypeTimer = dstType(shared.DestinationType_TIMER)
)

const (
	minOpenExtentsForDstDLQ                      = 1
	maxExtentsToConsumeForDstDLQ                 = 2
	minOpenExtentsForDstTimer                    = 2
	defaultMinOpenPublishExtents                 = 2 // Only used if the extent configuration can't be retrieved
	defaultRemoteExtents                         = 2
	defaultMinConsumeExtents                     = defaultMinOpenPublishExtents * 2
	maxExtentsToConsumeForDstTimer               = 64 // timer dst need to consume from all open extents
	minExtentsToConsumeForSingleCGVisibleExtents = 1
	replicatorCallTimeout                        = 20 * time.Second
)

var (
	// ErrTooManyUnHealthy is returned when there are too many open but unhealthy extents for a destination
	ErrTooManyUnHealthy = &shared.InternalServiceError{Message: "Too many open, but unhealthy extents for destination"}
)

var (
	// TTL after which the cache entry is due for refresh
	// The entry won't be evicted immediately after the TTL
	// We can keep serving stale entries for up to an hour,
	// when we cannot refresh the cache (say, due to cassandra failure)
	inputCacheTTL = int64(time.Second)
)

type (
	extentUUID                string
	extentMap                 map[extentUUID]*shared.Extent
	singleCGVisibleCacheEntry struct {
		ts                     common.UnixNanoTime // Time that entry was added to fetchSingleCGVisibleExtentsCache
		singleCGVisibleExtents extentMap
	}
)

var fetchSingleCGVisibleExtentsCache = make(map[string]*singleCGVisibleCacheEntry) // Key is cgUUID

func isUUIDLengthValid(uuid string) bool {
	return len(uuid) == common.UUIDStringLength
}

func isInputHealthy(context *Context, extent *shared.Extent) bool {
	return context.rpm.IsHostHealthy(common.InputServiceName, extent.GetInputHostUUID())
}

func isExtentBeingSealed(context *Context, extentID string) bool {
	return context.extentSeals.inProgress.Contains(extentID) || context.extentSeals.failed.Contains(extentID)
}

func getLockTimeout(result *resultCacheReadResult) time.Duration {
	if len(result.cachedResult) < 1 {
		return time.Second
	}
	return time.Duration(0)
}

func isAnyStoreHealthy(context *Context, storeIDs []string) bool {
	for _, id := range storeIDs {
		if context.rpm.IsHostHealthy(common.StoreServiceName, id) {
			return true
		}
	}
	return false
}

func areExtentStoresHealthy(context *Context, extent *shared.Extent) bool {
	for _, h := range extent.GetStoreUUIDs() {
		if !context.rpm.IsHostHealthy(common.StoreServiceName, h) {
			context.log.WithFields(bark.Fields{
				common.TagExt:  common.FmtExt(extent.GetExtentUUID()),
				common.TagStor: common.FmtStor(h),
			}).Info("Found unhealthy extent, store unhealthy")
			return false
		}
	}
	return true
}

func addExtentDownEvent(context *Context, sealSeq int64, dstID string, extentID string) {
	if !context.extentSeals.inProgress.PutIfNotExist(extentID, Boolean(true)) {
		return
	}
	event := NewExtentDownEvent(sealSeq, dstID, extentID)
	if !context.eventPipeline.Add(event) {
		context.extentSeals.inProgress.Remove(extentID)
	}
}

func addStoreExtentStatusOutOfSyncEvent(context *Context, dstID string, extentID string, storeID string) {
	if !context.extentSeals.inProgress.PutIfNotExist(extentID, Boolean(true)) {
		return
	}
	event := NewStoreExtentStatusOutOfSyncEvent(dstID, extentID, storeID, shared.ExtentStatus_SEALED)
	if !context.eventPipeline.Add(event) {
		context.extentSeals.inProgress.Remove(extentID)
	}
}

func checkCGEExists(context *Context, dstUUID, cgUUID string, extUUID extentUUID, m3Scope int) bool {
	_, e := context.mm.ReadConsumerGroupExtent(dstUUID, cgUUID, string(extUUID))
	if e == nil {
		// CGExtent already added, move on
		context.log.WithField(common.TagExt, common.FmtExt(string(extUUID))).Warn("Cassandra inconsistency detected")
		return true
	}
	if _, ok := e.(*shared.EntityNotExistsError); !ok { // EntityNotExists is expected; for other errors, just give up on this extent
		// Skip adding this extent and move along
		context.m3Client.IncCounter(m3Scope, metrics.ControllerErrMetadataReadCounter)
		return true
	}
	return false
}

func validateDstStatus(dstDesc *shared.DestinationDescription) error {
	switch dstDesc.GetStatus() {
	case shared.DestinationStatus_ENABLED:
		return nil
	case shared.DestinationStatus_DELETED:
		return ErrDestinationNotExists
	case shared.DestinationStatus_DELETING:
		return ErrDestinationNotExists
	default:
		return ErrDestinationDisabled
	}
}

func listConsumerGroupExtents(context *Context, dstUUID string, cgUUID string, m3Scope int, filterByStatus []m.ConsumerGroupExtentStatus) ([]*m.ConsumerGroupExtent, error) {
	cgExtents, err := context.mm.ListExtentsByConsumerGroup(dstUUID, cgUUID, filterByStatus)
	if err != nil {
		context.m3Client.IncCounter(m3Scope, metrics.ControllerErrMetadataReadCounter)
	}
	return cgExtents, err
}

func isEntityError(err error) bool {
	switch err.(type) {
	case *shared.EntityNotExistsError:
		return true
	case *shared.EntityDisabledError:
		return true
	}
	return false
}

func readDestination(context *Context, dstID string, m3Scope int) (*shared.DestinationDescription, error) {
	dstDesc, err := context.mm.ReadDestination(dstID, "")
	if err != nil {
		if _, ok := err.(*shared.EntityNotExistsError); !ok {
			context.m3Client.IncCounter(m3Scope, metrics.ControllerErrMetadataEntityNotFoundCounter)
		} else {
			context.m3Client.IncCounter(m3Scope, metrics.ControllerErrMetadataReadCounter)
		}
		return nil, err
	}
	return dstDesc, err
}

func findOpenExtents(context *Context, dstID string, m3Scope int) ([]*shared.ExtentStats, error) {
	filterBy := []shared.ExtentStatus{shared.ExtentStatus_OPEN}
	extentStats, err := context.mm.ListExtentsByDstIDStatus(dstID, filterBy)
	if err != nil {
		context.m3Client.IncCounter(m3Scope, metrics.ControllerErrMetadataReadCounter)
		return nil, err
	}
	return extentStats, err
}

func getDstType(desc *shared.DestinationDescription) dstType {
	dstType := desc.GetType()
	switch dstType {
	case shared.DestinationType_PLAIN:
		return dstTypePlain
	case shared.DestinationType_TIMER:
		return dstTypeTimer
	default:
		if common.PathDLQRegex.MatchString(desc.GetPath()) {
			return dstTypeDLQ
		}
	}
	return dstTypePlain
}

func minOpenExtentsForDst(context *Context, dstPath string, dstType dstType) int {
	switch dstType {
	case dstTypeTimer: // TODO: remove when timers are deprecated
		return minOpenExtentsForDstTimer
	case dstTypeDLQ:
		return minOpenExtentsForDstDLQ
	}

	logFn := func() bark.Logger {
		return context.log.WithField(common.TagDst, dstPath).WithField(common.TagModule, `extentAssign`)
	}

	cfgIface, err := context.cfgMgr.Get(common.ControllerServiceName, `*`, `*`, `*`)
	if err != nil {
		logFn().WithField(common.TagErr, err).Error(`Couldn't get extent target configuration`)
		return defaultMinOpenPublishExtents
	}

	cfg, ok := cfgIface.(ControllerDynamicConfig)
	if !ok {
		logFn().Error(`Couldn't cast cfg to ControllerDynamicConfig`)
		return defaultMinOpenPublishExtents
	}

	return int(common.OverrideValueByPrefix(logFn, dstPath, cfg.NumPublisherExtentsByPath, defaultMinOpenPublishExtents, `NumPublisherExtentsByPath`))
}

func getInputAddrIfExtentIsWritable(context *Context, extent *shared.Extent, m3Scope int) (string, error) {
	inputhost, err := context.rpm.ResolveUUID(common.InputServiceName, extent.GetInputHostUUID())
	if err != nil {
		context.log.
			WithField(common.TagExt, common.FmtExt(extent.GetExtentUUID())).
			WithField(common.TagIn, common.FmtIn(extent.GetInputHostUUID())).
			Info("Found unhealthy extent, input unhealthy")
		return "", err
	}
	if !areExtentStoresHealthy(context, extent) {
		return "", errNoStoreHosts
	}
	return inputhost, nil
}

func createExtent(context *Context, dstUUID string, isMultiZoneDest bool, m3Scope int) (extentUUID string, inhost *common.HostInfo, storehosts []*common.HostInfo, err error) {
	// TODO We also have a dst specific nReplicas param, do we need it ?
	var nReplicasPerExtent = int(context.appConfig.GetDestinationConfig().GetReplicas())

	storehosts, err = context.placement.PickStoreHosts(nReplicasPerExtent)
	if err != nil {
		context.m3Client.IncCounter(m3Scope, metrics.ControllerErrPickStoreHostCounter)
		return
	}

	storeids := make([]string, nReplicasPerExtent)
	for i := 0; i < nReplicasPerExtent; i++ {
		storeids[i] = storehosts[i].UUID
	}

	inhost, err = context.placement.PickInputHost(storehosts)
	if err != nil {
		context.m3Client.IncCounter(m3Scope, metrics.ControllerErrPickInHostCounter)
		return
	}

	extentUUID = uuid.New()
	_, err = context.mm.CreateExtent(dstUUID, extentUUID, inhost.UUID, storeids)
	if err != nil {
		context.m3Client.IncCounter(m3Scope, metrics.ControllerErrMetadataUpdateCounter)
		return
	}

	lclLg := context.log.WithFields(bark.Fields{
		common.TagDst: common.FmtDst(dstUUID),
		common.TagExt: common.FmtExt(extentUUID),
	})

	lclLg.WithFields(bark.Fields{
		common.TagIn: common.FmtIn(inhost.UUID),
		`storeids`:   storeids,
	}).Info("Extent Created locally")

	// Triggers async notifications to input host
	event := NewExtentCreatedEvent(dstUUID, inhost.UUID, extentUUID, storeids)
	context.eventPipeline.Add(event)

	if isMultiZoneDest {
		extent := &shared.Extent{
			ExtentUUID:      common.StringPtr(extentUUID),
			DestinationUUID: common.StringPtr(dstUUID),
			InputHostUUID:   common.StringPtr(common.InputHostForRemoteExtent),
			StoreUUIDs:      []string{},
			OriginZone:      common.StringPtr(context.localZone),
		}
		req := &shared.CreateExtentRequest{Extent: extent}

		// send to local replicator to fan out
		localReplicator, replicatorErr := context.clientFactory.GetReplicatorClient()
		if replicatorErr != nil {
			lclLg.Error(replicatorErr.Error())
			context.m3Client.IncCounter(m3Scope, metrics.ControllerErrCallReplicatorCounter)
			return
		}

		ctx, cancel := thrift.NewContext(replicatorCallTimeout)
		defer cancel()
		replicatorErr = localReplicator.CreateRemoteExtent(ctx, req)
		if replicatorErr != nil {
			lclLg.Error(replicatorErr.Error())
			context.m3Client.IncCounter(m3Scope, metrics.ControllerErrCallReplicatorCounter)
			return
		}

		lclLg.Info("Called replicator to Create Extent")
	}

	return
}

func refreshInputHostsForDst(context *Context, dstUUID string, now int64) ([]string, error) {

	var m3Scope = metrics.RefreshInputHostsForDstScope
	context.m3Client.IncCounter(m3Scope, metrics.ControllerRequests)

	dstDesc, err := readDestination(context, dstUUID, m3Scope)
	if err != nil {
		context.m3Client.IncCounter(m3Scope, metrics.ControllerFailures)
		return nil, err
	}

	if err = validateDstStatus(dstDesc); err != nil {
		return nil, err
	}

	var dstType = getDstType(dstDesc)
	var minOpenExtents = minOpenExtentsForDst(context, dstDesc.GetPath(), dstType)
	var isMultiZoneDest = dstDesc.GetIsMultiZone()

	openExtentStats, err := findOpenExtents(context, dstUUID, m3Scope)
	if err != nil {
		// we can't get the metadata, let's
		// continue to use the cached result
		// in the meanwhile i.e. cached entries
		// wont get deleted until we can talk
		// back to cassandra
		context.m3Client.IncCounter(m3Scope, metrics.ControllerFailures)
		return nil, err
	}

	var nHealthy = 0
	var inputHosts = make(map[string]*common.HostInfo, len(openExtentStats))

	for _, stat := range openExtentStats {
		ext := stat.GetExtent()

		// skip remote zone extent(read only)
		if common.IsRemoteZoneExtent(ext.GetOriginZone(), context.localZone) {
			continue
		}
		if isExtentBeingSealed(context, ext.GetExtentUUID()) {
			continue
		}
		addr, e := getInputAddrIfExtentIsWritable(context, ext, m3Scope)
		if e != nil {
			continue
		}
		hostID := ext.GetInputHostUUID()
		hostInfo := &common.HostInfo{UUID: hostID, Addr: addr}
		inputHosts[hostID] = hostInfo
		nHealthy++
	}

	var ttl = inputCacheTTL
	var newHost *common.HostInfo
	var backoffRetryTTL = int64(100 * time.Millisecond)

	if nHealthy >= minOpenExtents {
		goto done
	}

	_, newHost, _, err = createExtent(context, dstUUID, isMultiZoneDest, m3Scope)
	if err == nil {
		inputHosts[newHost.UUID] = newHost
		nHealthy++
	}

	if nHealthy == 0 {
		// intermittent cassandra error
		// force ttl to backoff interval
		// to avoid retrying in a loop
		ttl = backoffRetryTTL
		context.m3Client.IncCounter(m3Scope, metrics.ControllerFailures)
		goto done
	}

done:
	expiry := now + ttl
	uuids, addrs := hostInfoMapToSlice(inputHosts)
	context.resultCache.write(dstUUID, resultCacheParams{
		dstType:    dstType,
		nExtents:   nHealthy,
		maxExtents: minOpenExtents,
		hostIDs:    uuids,
		expiry:     expiry,
	})

	return addrs, nil
}
