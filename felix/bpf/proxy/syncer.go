// Copyright (c) 2017-2021 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	k8sp "k8s.io/kubernetes/pkg/proxy"

	"github.com/projectcalico/calico/felix/bpf"
	"github.com/projectcalico/calico/felix/bpf/maps"
	"github.com/projectcalico/calico/felix/bpf/nat"
	"github.com/projectcalico/calico/felix/bpf/routes"
	"github.com/projectcalico/calico/felix/cachingmap"
	"github.com/projectcalico/calico/felix/ip"
)

var podNPIPStr = "255.255.255.255"
var podNPIP = net.ParseIP(podNPIPStr)
var podNPIPV6Str = "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"
var podNPIPV6 = net.ParseIP(podNPIPV6Str)

// Service combines k8s service properties with the service annotations
type Service interface {
	k8sp.ServicePort
	ServiceAnnotations
}

type svcInfo struct {
	id         uint32
	count      int
	localCount int
	svc        Service
}

type svcKey struct {
	sname k8sp.ServicePortName
	extra string
}

func (k svcKey) String() string {
	if k.extra == "" {
		return k.sname.String()
	}

	return fmt.Sprintf("%s:%s", k.extra, k.sname)
}

func getSvcKey(sname k8sp.ServicePortName, extra string) svcKey {
	return svcKey{
		sname: sname,
		extra: extra,
	}
}

type svcType int

const (
	svcTypeExternalIP svcType = iota
	svcTypeNodePort
	svcTypeNodePortRemote
	svcTypeLoadBalancer
)

var svcType2String = map[svcType]string{
	svcTypeNodePort:       "NodePort",
	svcTypeExternalIP:     "ExternalIP",
	svcTypeNodePortRemote: "NodePortRemote",
	svcTypeLoadBalancer:   "LoadBalancer",
}

func getSvcKeyExtra(t svcType, ip net.IP) string {
	return svcType2String[t] + ":" + ip.String()
}

func hasSvcKeyExtra(skey svcKey, t svcType) bool {
	return strings.HasPrefix(skey.extra, svcType2String[t]+":")
}

func isSvcKeyDerived(skey svcKey) bool {
	return hasSvcKeyExtra(skey, svcTypeExternalIP) ||
		hasSvcKeyExtra(skey, svcTypeNodePort) ||
		hasSvcKeyExtra(skey, svcTypeLoadBalancer)
}

type stickyFrontend struct {
	id    uint32
	timeo time.Duration
}

// Syncer is an implementation of DPSyncer interface. It is not thread safe and
// should be called only once at a time
type Syncer struct {
	ipFamily int

	bpfSvcs *cachingmap.CachingMap[nat.FrontendKeyInterface, nat.FrontendValue]
	bpfEps  *cachingmap.CachingMap[nat.BackendKey, nat.BackendValueInterface]
	bpfAff  maps.Map

	nextSvcID uint32

	nodePortIPs []net.IP
	rt          Routes

	// new maps are valid during the Apply()'s runtime to provide easy access
	// to updating them. They become prev at the end of it to be compared
	// against in the next iteration
	newSvcMap  map[svcKey]svcInfo
	newEpsMap  k8sp.EndpointsMap
	prevSvcMap map[svcKey]svcInfo
	prevEpsMap k8sp.EndpointsMap
	// active Maps contain all active svcs endpoints at the end of an iteration
	activeSvcsMap map[ipPortProto]uint32
	activeEpsMap  map[uint32]map[ipPort]struct{}

	// Protects accessing the [prev|new][Svc|Eps]Map,
	mapsLck sync.Mutex

	// synced is true after reconciling the first Apply
	synced bool

	expFixupWg   sync.WaitGroup
	expFixupStop chan struct{}

	stop     chan struct{}
	stopOnce sync.Once

	stickySvcs map[nat.FrontEndAffinityKeyInterface]stickyFrontend
	stickyEps  map[uint32]map[nat.BackendValueInterface]struct{}

	// triggerFn is called when one of the syncer's background threads needs to trigger an Apply().
	// The proxy sets this to the runner's Run() method.  We assume that the method doesn't block.
	triggerFn func()

	newFrontendKey         func(addr net.IP, port uint16, protocol uint8) nat.FrontendKeyInterface
	newFrontendKeySrc      func(addr net.IP, port uint16, protocol uint8, cidr ip.CIDR) nat.FrontendKeyInterface
	newBackendValue        func(addr net.IP, port uint16) nat.BackendValueInterface
	affinityKeyFromBytes   func([]byte) nat.AffinityKeyInterface
	affinityValueFromBytes func([]byte) nat.AffinityValueInterface

	excludedCIDRs *ip.CIDRTrie
}

type ipPort struct {
	ip   string
	port int
}

type ipPortProto struct {
	ipPort
	proto uint8
}

// servicePortToIPPortProto is a simple way how to turn a k8sp.ServicePort into
// an ipPortProto
func servicePortToIPPortProto(sp k8sp.ServicePort) ipPortProto {
	return ipPortProto{
		ipPort: ipPort{
			ip:   sp.ClusterIP().String(),
			port: sp.Port(),
		},
		proto: ProtoV1ToIntPanic(sp.Protocol()),
	}
}

func uniqueIPs(ips []net.IP) []net.IP {
	m := make(map[string]net.IP)
	unique := true

	for _, ip := range ips {
		s := ip.String()
		if _, ok := m[s]; ok {
			unique = false
		} else {
			m[s] = ip
		}
	}

	if unique {
		return ips
	}

	ret := make([]net.IP, 0, len(m))
	for _, ip := range m {
		ret = append(ret, ip)
	}

	return ret
}

// NewSyncer returns a new Syncer
func NewSyncer(family int, nodePortIPs []net.IP,
	frontendMap maps.MapWithExistsCheck, backendMap maps.MapWithExistsCheck,
	affmap maps.Map, rt Routes,
	excludedCIDRs *ip.CIDRTrie,
) (*Syncer, error) {

	s := &Syncer{
		ipFamily:      family,
		bpfAff:        affmap,
		rt:            rt,
		nodePortIPs:   uniqueIPs(nodePortIPs),
		prevSvcMap:    make(map[svcKey]svcInfo),
		prevEpsMap:    make(k8sp.EndpointsMap),
		stop:          make(chan struct{}),
		excludedCIDRs: excludedCIDRs,
	}

	switch family {
	case 4:
		s.bpfSvcs = cachingmap.New[nat.FrontendKeyInterface, nat.FrontendValue](frontendMap.GetName(),
			maps.NewTypedMap[nat.FrontendKeyInterface, nat.FrontendValue](
				frontendMap, nat.FrontendKeyFromBytes, nat.FrontendValueFromBytes,
			))
		s.bpfEps = cachingmap.New[nat.BackendKey, nat.BackendValueInterface](backendMap.GetName(),
			maps.NewTypedMap[nat.BackendKey, nat.BackendValueInterface](
				backendMap, nat.BackendKeyFromBytes, nat.BackendValueFromBytes,
			))
		s.newFrontendKey = nat.NewNATKeyIntf
		s.newFrontendKeySrc = nat.NewNATKeySrcIntf
		s.newBackendValue = nat.NewNATBackendValueIntf
		s.affinityKeyFromBytes = nat.AffinityKeyIntfFromBytes
		s.affinityValueFromBytes = nat.AffinityValueIntfFromBytes
	case 6:
		s.bpfSvcs = cachingmap.New[nat.FrontendKeyInterface, nat.FrontendValue](frontendMap.GetName(),
			maps.NewTypedMap[nat.FrontendKeyInterface, nat.FrontendValue](
				frontendMap, nat.FrontendKeyV6FromBytes, nat.FrontendValueFromBytes,
			))
		s.bpfEps = cachingmap.New[nat.BackendKey, nat.BackendValueInterface](backendMap.GetName(),
			maps.NewTypedMap[nat.BackendKey, nat.BackendValueInterface](
				backendMap, nat.BackendKeyFromBytes, nat.BackendValueV6FromBytes,
			))
		s.newFrontendKey = nat.NewNATKeyV6Intf
		s.newFrontendKeySrc = nat.NewNATKeyV6SrcIntf
		s.newBackendValue = nat.NewNATBackendValueV6Intf
		s.affinityKeyFromBytes = nat.AffinityKeyV6IntfFromBytes
		s.affinityValueFromBytes = nat.AffinityValueV6IntfFromBytes
	default:
		return nil, fmt.Errorf("unknwn family %d", family)
	}

	return s, nil
}

func (s *Syncer) loadOrigs() error {
	err := s.bpfEps.LoadCacheFromDataplane()
	if err != nil {
		return err
	}
	err = s.bpfSvcs.LoadCacheFromDataplane()
	if err != nil {
		return err
	}
	return nil
}

type syncRef struct {
	svc  k8sp.ServicePortName
	info k8sp.ServicePort
}

// svcMapToIPPortProtoMap takes the kubernetes service representation and makes an index
// so we can cross reference with the values we learn from the dataplane.
func (s *Syncer) svcMapToIPPortProtoMap(svcs k8sp.ServicePortMap) map[nat.FrontendKeyInterface]syncRef {
	ref := make(map[nat.FrontendKeyInterface]syncRef, len(svcs))

	for key, svc := range svcs {
		clusterIP := svc.ClusterIP()
		proto := uint8(ProtoV1ToIntPanic(svc.Protocol()))
		port := uint16(svc.Port())

		xref := syncRef{key, svc}

		ref[s.newFrontendKey(clusterIP, port, proto)] = xref

		np := uint16(0)

		if svc.NodePort() != 0 {
			np = uint16(svc.NodePort())

			ref[s.newFrontendKey(clusterIP, np, proto)] = xref

			for _, npIP := range s.nodePortIPs {
				ref[s.newFrontendKey(npIP, np, proto)] = xref
			}
		}

		for _, extIP := range svc.ExternalIPs() {
			ref[s.newFrontendKey(extIP, port, proto)] = xref
		}
	}

	return ref
}

func (s *Syncer) startupBuildPrev(state DPSyncerState) error {
	// Build a map keyed by nat.FrontendKey of services to be generated from the
	// state. The map values contains references to both ServicePortName keys of
	// the state map as well as the ServicePort values.
	svcRef := s.svcMapToIPPortProtoMap(state.SvcMap)

	inconsistent := false

	// Walk the frontend bpf map that was read into memory and match it against the
	// references build from the state
	s.bpfSvcs.Dataplane().Iter(func(svck nat.FrontendKeyInterface, svcv nat.FrontendValue) {
		xref, ok := svcRef[svck]
		if !ok {
			return
		}

		// If there is a cross-reference with the current state, try to match
		// what is in the bpf map with what was supposed to be a service that
		// created it - based on the current state.
		svckey := s.matchBpfSvc(svck, xref.svc, xref.info)
		if svckey == nil {
			return
		}

		id := svcv.ID()
		count := int(svcv.Count())
		s.prevSvcMap[*svckey] = svcInfo{
			id:         id,
			count:      count,
			localCount: int(svcv.LocalCount()),
			svc:        state.SvcMap[svckey.sname].(Service),
		}

		if id >= s.nextSvcID {
			s.nextSvcID = id + 1
		}

		if svckey.extra != "" {
			return
		}

		if count > 0 {
			s.prevEpsMap[svckey.sname] = make([]k8sp.Endpoint, 0, count)
		}
		for i := 0; i < count; i++ {
			epk := nat.NewNATBackendKey(id, uint32(i))
			ep, ok := s.bpfEps.Dataplane().Get(epk)
			if !ok {
				log.Warnf("inconsistent backed map, missing ep %s", epk)
				inconsistent = true
				break
			}
			s.prevEpsMap[svckey.sname] = append(s.prevEpsMap[svckey.sname],
				NewEndpointInfo(ep.Addr().String(), int(ep.Port())),
			)
		}
	})

	if inconsistent {
		return fmt.Errorf("found inconsistencies in existing BPF maps, will rewrite maps from scratch")
	}

	return nil
}

func (s *Syncer) startupSync(state DPSyncerState) error {
	// Load current dataplane state.
	if err := s.loadOrigs(); err != nil {
		return err
	}

	// Try to build the previous maps based on the current state and what is in bpf maps.
	// Once we have the previous map, we can apply the current state as if we never
	// restarted and apply only the diff using the regular code path.
	if err := s.startupBuildPrev(state); err != nil {
		log.WithError(err).Error("Failed to load previous state of NAT maps from dataplane, " +
			"maps will get disruptively rewritten")
	}
	return nil
}

func (s *Syncer) applySvc(skey svcKey, sinfo Service, eps []k8sp.Endpoint) error {
	var id uint32

	old, exists := s.prevSvcMap[skey]
	if exists && ServicePortEqual(old.svc, sinfo) {
		id = old.id
	} else {
		id = s.newSvcID()
	}
	count, local, err := s.updateService(skey, sinfo, id, eps)
	if err != nil {
		return err
	}

	s.newSvcMap[skey] = svcInfo{
		id:         id,
		count:      count,
		localCount: local,
		svc:        sinfo,
	}

	if log.GetLevel() >= log.DebugLevel {
		log.Debugf("applied a service %s update: sinfo=%+v", skey, s.newSvcMap[skey])
	}

	return nil
}

func (s *Syncer) addActiveEps(id uint32, svc Service, eps []k8sp.Endpoint) {
	svcKey := servicePortToIPPortProto(svc)

	s.activeSvcsMap[svcKey] = id

	if len(eps) == 0 {
		return
	}

	epsmap := make(map[ipPort]struct{})
	s.activeEpsMap[id] = epsmap
	for _, ep := range eps {
		if ep.IsTerminating() && svc.Protocol() == v1.ProtocolUDP && svc.ReapTerminatingUDP() {
			continue // do not add this endpoint, treat it as if does not exist anymore
		}
		port := ep.Port() // it is error free by this point
		epsmap[ipPort{
			ip:   ep.IP(),
			port: port,
		}] = struct{}{}
	}
}

func (s *Syncer) applyExpandedNP(sname k8sp.ServicePortName, sinfo k8sp.ServicePort,
	eps []k8sp.Endpoint, node ip.Addr, nport int) error {
	skey := getSvcKey(sname, getSvcKeyExtra(svcTypeNodePortRemote, node.AsNetIP()))
	si := serviceInfoFromK8sServicePort(sinfo)
	si.clusterIP = node.AsNetIP()
	si.port = nport

	if err := s.applySvc(skey, si, eps); err != nil {
		return fmt.Errorf("apply NodePortRemote for %s node %s", sname, node)
	}

	return nil
}

type expandMiss struct {
	sname k8sp.ServicePortName
	sinfo k8sp.ServicePort
	eps   []k8sp.Endpoint
	nport int
}

func (s *Syncer) expandAndApplyNodePorts(sname k8sp.ServicePortName, sinfo k8sp.ServicePort,
	eps []k8sp.Endpoint, nport int, rtLookup func(addr ip.Addr) (routes.ValueInterface, bool)) *expandMiss {

	ipToEp, miss := s.expandNodePorts(sname, sinfo, eps, nport, rtLookup)

	for node, neps := range ipToEp {
		if err := s.applyExpandedNP(sname, sinfo, neps, node, nport); err != nil {
			log.WithField("error", err).Errorf("Failed to expand NodePort")
		}
	}

	return miss
}

func (s *Syncer) expandNodePorts(
	sname k8sp.ServicePortName,
	sinfo k8sp.ServicePort,
	eps []k8sp.Endpoint,
	nport int,
	rtLookup func(addr ip.Addr) (routes.ValueInterface, bool),
) (map[ip.Addr][]k8sp.Endpoint, *expandMiss) {
	ipToEp := make(map[ip.Addr][]k8sp.Endpoint)
	var miss *expandMiss
	for _, ep := range eps {
		ipa := ip.FromString(ep.IP())

		rt, ok := rtLookup(ipa)
		if !ok {
			log.Errorf("No route for %s", ipa)
			if miss == nil {
				miss = &expandMiss{
					sname: sname,
					sinfo: sinfo,
					nport: nport,
				}
			}
			miss.eps = append(miss.eps, ep)
			continue
		}

		flags := rt.Flags()
		// Include only remote workloads.
		if flags&routes.FlagWorkload != 0 && flags&routes.FlagLocal == 0 {
			nodeIP := rt.NextHop()

			ipToEp[nodeIP] = append(ipToEp[nodeIP], ep)
			if log.GetLevel() >= log.DebugLevel {
				log.Debugf("found rt %s for remote dest %s", nodeIP, ipa)
			}
		}
	}
	return ipToEp, miss
}

func (s *Syncer) applyDerived(
	sname k8sp.ServicePortName,
	t svcType,
	sinfo Service,
) error {

	svc, ok := s.newSvcMap[getSvcKey(sname, "")]
	if !ok {
		// this should not happen
		return fmt.Errorf("no ClusterIP for derived service type %d", t)
	}

	var skey svcKey
	count := svc.count
	local := svc.localCount

	skey = getSvcKey(sname, getSvcKeyExtra(t, sinfo.ClusterIP()))
	flags := uint32(0)

	switch t {
	case svcTypeNodePort, svcTypeLoadBalancer, svcTypeNodePortRemote:
		if sinfo.ExternalPolicyLocal() {
			flags |= nat.NATFlgExternalLocal
		}
		if sinfo.InternalPolicyLocal() {
			flags |= nat.NATFlgInternalLocal
		}
	}

	newInfo := svcInfo{
		id:         svc.id,
		count:      count,
		localCount: local,
		svc:        sinfo,
	}

	if err := s.writeSvc(sinfo, svc.id, count, local, flags); err != nil {
		return err
	}
	if svcTypeLoadBalancer == t || svcTypeExternalIP == t {
		err := s.writeLBSrcRangeSvcNATKeys(sinfo, svc.id, count, local, flags)
		if err != nil {
			log.Debug("Failed to write LB source range NAT keys")
		}
	}

	s.newSvcMap[skey] = newInfo
	if log.GetLevel() >= log.DebugLevel {
		log.Debugf("applied a derived service %s update: sinfo=%+v", skey, s.newSvcMap[skey])
	}

	return nil
}

func (s *Syncer) apply(state DPSyncerState) error {
	log.Infof("Applying new state, %d service", len(state.SvcMap))
	log.Debugf("Applying new state, %v", state)

	// we need to copy the maps from the new state to compute the diff in the
	// next call. We cannot keep the provided maps as the generic k8s proxy code
	// updates them. This function is called with a lock held so we are safe
	// here and now.
	s.newSvcMap = make(map[svcKey]svcInfo, len(state.SvcMap))
	s.newEpsMap = make(k8sp.EndpointsMap, len(state.EpsMap))
	nodeZone := state.NodeZone

	var expNPMisses []*expandMiss

	// Start with a completely empty slate (in memory).  We'll then repopulate both maps from scratch and
	// let CachingMap calculate deltas...
	s.bpfSvcs.Desired().DeleteAll()
	s.bpfEps.Desired().DeleteAll()

	// insert or update existing services
	for sname, sinfo := range state.SvcMap {
		svc := sinfo.(Service)
		hintsAnnotation := svc.HintsAnnotation()

		log.WithField("service", sname).Debug("Applying service")
		skey := getSvcKey(sname, "")

		eps := make([]k8sp.Endpoint, 0, len(state.EpsMap[sname]))
		for _, ep := range state.EpsMap[sname] {
			zoneHints := ep.ZoneHints()
			if ep.IsReady() || ep.IsTerminating() {
				if ShouldAppendTopologyAwareEndpoint(nodeZone, hintsAnnotation, zoneHints) {
					eps = append(eps, ep)
				} else {
					log.Debugf("Topology Aware Hints: '%s' for Endpoint: '%s' however Zone: '%s' does not match Zone Hints: '%v'\n",
						hintsAnnotation,
						ep.IP(),
						nodeZone,
						zoneHints)
				}
			}
		}

		err := s.applySvc(skey, svc, eps)
		if err != nil {
			return err
		}

		for _, lbIP := range svc.LoadBalancerVIPs() {
			if len(lbIP) != 0 {
				extInfo := serviceInfoFromK8sServicePort(svc)
				extInfo.clusterIP = lbIP
				err := s.applyDerived(sname, svcTypeLoadBalancer, extInfo)
				if err != nil {
					log.Errorf("failed to apply LoadBalancer IP %s for service %s : %s", lbIP, sname, err)
					continue
				}
				log.Debugf("LB status IP %s", lbIP)
			}
		}
		// N.B. we assume that k8s provide us with no duplicities
		for _, extIP := range svc.ExternalIPs() {
			extInfo := serviceInfoFromK8sServicePort(svc)
			extInfo.clusterIP = extIP
			err := s.applyDerived(sname, svcTypeExternalIP, extInfo)
			if err != nil {
				log.Errorf("failed to apply ExternalIP %s for service %s : %s", extIP, sname, err)
				continue
			}
		}

		if nport := svc.NodePort(); nport != 0 {
			for _, npip := range s.nodePortIPs {
				npInfo := serviceInfoFromK8sServicePort(svc)
				npInfo.clusterIP = npip
				npInfo.port = nport
				if svc.InternalPolicyLocal() &&
					((s.ipFamily == 4 && npip.Equal(podNPIP)) || (s.ipFamily == 6 && npip.Equal(podNPIPV6))) {
					// do not program the meta entry, program each node
					// separately
					continue
				}
				err := s.applyDerived(sname, svcTypeNodePort, npInfo)
				if err != nil {
					log.Errorf("failed to apply NodePort %s for service %s : %s", npip, sname, err)
					continue
				}
			}
			if svc.InternalPolicyLocal() {
				if miss := s.expandAndApplyNodePorts(sname, svc, eps, nport, s.rt.Lookup); miss != nil {
					expNPMisses = append(expNPMisses, miss)
				}
			}
		}
	}

	// Delete any front-ends first so the backends become unreachable.
	err := s.bpfSvcs.ApplyDeletionsOnly()
	if err != nil {
		return err
	}
	// Update the backend maps so that any new backends become available before we update the frontends to use them.
	err = s.bpfEps.ApplyUpdatesOnly()
	if err != nil {
		return err
	}
	// Update the frontends, after this is done we should be handling packets correctly.
	err = s.bpfSvcs.ApplyUpdatesOnly()
	if err != nil {
		return err
	}
	// Remove any unused backends.
	err = s.bpfEps.ApplyDeletionsOnly()
	if err != nil {
		return err
	}

	log.Info("new state written")

	s.runExpandNPFixup(expNPMisses)

	return nil
}

// Apply applies the new state
func (s *Syncer) Apply(state DPSyncerState) error {
	if !s.synced {
		log.Infof("Loading BPF map state from dataplane")
		if err := s.startupSync(state); err != nil {
			return errors.WithMessage(err, "startup sync")
		}
		log.Infof("Loaded BPF map state from dataplane")
		s.mapsLck.Lock()
	} else {
		// if we were not synced yet, the fixer cannot run yet
		s.StopExpandNPFixup()

		s.mapsLck.Lock()
		s.prevSvcMap = s.newSvcMap
		s.prevEpsMap = s.newEpsMap
	}

	defer s.mapsLck.Unlock()

	// preallocate maps to track sticky services for cleanup
	s.stickySvcs = make(map[nat.FrontEndAffinityKeyInterface]stickyFrontend)
	s.stickyEps = make(map[uint32]map[nat.BackendValueInterface]struct{})

	defer func() {
		// not needed anymore
		s.stickySvcs = nil
		s.stickyEps = nil
	}()

	if err := s.apply(state); err != nil {
		// dont bother to cleanup affinity since we do not know in what state we
		// are anyway. Will get resolved once we get in a good state
		return err
	}

	// we are fully synced now
	if !s.synced {
		s.synced = true
	}

	// We wrote all updates, no one will create new records in affinity table
	// that we would clean up now, so do it!
	return s.cleanupSticky()
}

func (s *Syncer) updateService(skey svcKey, sinfo Service, id uint32, eps []k8sp.Endpoint) (int, int, error) {
	cpEps := make([]k8sp.Endpoint, 0, len(eps))

	cnt := 0
	local := 0

	if sinfo.SessionAffinityType() == v1.ServiceAffinityClientIP {
		// since we write the backend before we write the frontend, we need to
		// preallocate the map for it
		s.stickyEps[id] = make(map[nat.BackendValueInterface]struct{})
	}

	for _, ep := range eps {
		if !ep.IsLocal() {
			continue
		}

		// eps could contain Ready and Terminating pods but only write Ready pods to backend.
		if ep.IsReady() {
			if err := s.writeSvcBackend(id, uint32(cnt), ep); err != nil {
				return 0, 0, err
			}
			cnt++
			local++
		}

		cpEps = append(cpEps, ep)
	}

	for _, ep := range eps {
		if ep.IsLocal() {
			continue
		}

		// eps could contain Ready and Terminating pods but only write Ready pods to backend.
		if ep.IsReady() {
			if err := s.writeSvcBackend(id, uint32(cnt), ep); err != nil {
				return 0, 0, err
			}
			cnt++
		}

		cpEps = append(cpEps, ep)
	}

	flags := uint32(0)
	if sinfo.InternalPolicyLocal() {
		flags |= nat.NATFlgInternalLocal
	}

	if err := s.writeSvc(sinfo, id, cnt, local, flags); err != nil {
		return 0, 0, err
	}

	// svcTypeNodePortRemote is semi-primary service - it has a different set of
	// backends for NAT (hence primary) but is also derived and would overwrite
	// the primary service for connection cleaning.
	//
	// As a result we are a bit more conservative in which connections we break.
	// Even if a backend is technically not reachable through the nodeport due
	// to the Local vs. Cluster traffic policy, there is no harm if include also
	// those backends and possible do not break connections that cannot happen.
	if !hasSvcKeyExtra(skey, svcTypeNodePortRemote) {
		s.newEpsMap[skey.sname] = cpEps
	}

	return cnt, local, nil
}

func (s *Syncer) writeSvcBackend(svcID uint32, idx uint32, ep k8sp.Endpoint) error {
	if log.GetLevel() >= log.DebugLevel {
		log.WithFields(log.Fields{
			"svcID": svcID,
			"idx":   idx,
			"ep":    ep,
		}).Debug("Writing service backend.")
	}
	ip := net.ParseIP(ep.IP())

	key := nat.NewNATBackendKey(svcID, uint32(idx))

	tgtPort := ep.Port()
	val := s.newBackendValue(ip, uint16(tgtPort))
	s.bpfEps.Desired().Set(key, val)

	if s.stickyEps[svcID] != nil {
		s.stickyEps[svcID][val] = struct{}{}
	}

	return nil
}

func (s *Syncer) getSvcNATKey(svc k8sp.ServicePort) (nat.FrontendKeyInterface, error) {
	ip := svc.ClusterIP()
	port := svc.Port()
	proto, err := ProtoV1ToInt(svc.Protocol())
	if err != nil {
		return s.newFrontendKey(ip, uint16(port), proto), err
	}

	key := s.newFrontendKey(ip, uint16(port), proto)
	return key, nil
}

func (s *Syncer) getSvcNATKeyLBSrcRange(svc k8sp.ServicePort) ([]nat.FrontendKeyInterface, error) {
	ipaddr := svc.ClusterIP()
	port := svc.Port()
	loadBalancerSourceRanges := svc.LoadBalancerSourceRanges()
	if log.GetLevel() >= log.DebugLevel {
		log.Debugf("loadbalancer %v", loadBalancerSourceRanges)
	}
	proto, err := ProtoV1ToInt(svc.Protocol())
	if err != nil {
		return nil, err
	}

	keys := make([]nat.FrontendKeyInterface, 0, len(loadBalancerSourceRanges))

	for _, src := range loadBalancerSourceRanges {
		if src != nil && src.IP.To4() == nil {
			if s.ipFamily != 6 {
				continue
			}
		} else if s.ipFamily == 6 {
			continue
		}
		key := s.newFrontendKeySrc(ipaddr, uint16(port), proto, ip.CIDRFromIPNet(src))
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *Syncer) writeLBSrcRangeSvcNATKeys(svc k8sp.ServicePort, svcID uint32, count, local int, flags uint32) error {
	var key nat.FrontendKeyInterface
	affinityTimeo := uint32(0)
	if svc.SessionAffinityType() == v1.ServiceAffinityClientIP {
		affinityTimeo = uint32(svc.StickyMaxAgeSeconds())
	}

	if len(svc.LoadBalancerSourceRanges()) == 0 {
		return nil
	}
	keys, err := s.getSvcNATKeyLBSrcRange(svc)
	if err != nil {
		return err
	}
	val := nat.NewNATValueWithFlags(svcID, uint32(count), uint32(local), affinityTimeo, flags)
	for _, key := range keys {
		if log.GetLevel() >= log.DebugLevel {
			log.Debugf("bpf map writing %s:%s", key, val)
		}
		s.bpfSvcs.Desired().Set(key, val)
	}
	key, err = s.getSvcNATKey(svc)
	if err != nil {
		return err
	}
	val = nat.NewNATValue(svcID, nat.BlackHoleCount, uint32(0), uint32(0))
	s.bpfSvcs.Desired().Set(key, val)
	return nil
}

func (s *Syncer) writeSvc(svc Service, svcID uint32, count, local int, flags uint32) error {
	key, err := s.getSvcNATKey(svc)
	if err != nil {
		return err
	}

	if s.excludedCIDRs != nil {
		_, v := s.excludedCIDRs.LPM(ip.CIDRFromNetIP(svc.ClusterIP()))
		if v != nil {
			flags |= nat.NATFlgExclude
		}
	}
	if svc.ExcludeService() {
		flags |= nat.NATFlgExclude
	}

	affinityTimeo := uint32(0)
	if svc.SessionAffinityType() == v1.ServiceAffinityClientIP {
		affinityTimeo = uint32(svc.StickyMaxAgeSeconds())
	}

	val := nat.NewNATValueWithFlags(svcID, uint32(count), uint32(local), affinityTimeo, flags)

	if log.GetLevel() >= log.DebugLevel {
		log.Debugf("bpf map writing %s:%s", key, val)
	}
	s.bpfSvcs.Desired().Set(key, val)

	// we must have written the backends by now so the map exists
	if s.stickyEps[svcID] != nil {
		affkey := key.AffinityKeyCopy()
		s.stickySvcs[affkey] = stickyFrontend{
			id:    svcID,
			timeo: time.Duration(affinityTimeo) * time.Second,
		}
	}

	return nil
}

// ProtoV1ToInt translates k8s v1.Protocol to its IANA number and returns
// error if the proto is not recognized
func ProtoV1ToInt(p v1.Protocol) (uint8, error) {
	switch p {
	case v1.ProtocolTCP:
		return 6, nil
	case v1.ProtocolUDP:
		return 17, nil
	case v1.ProtocolSCTP:
		return 132, nil
	}

	return 0, fmt.Errorf("unknown protocol %q", p)
}

// ProtoV1ToIntPanic translates k8s v1.Protocol to its IANA number and panics if
// the protocol is not recognized
func ProtoV1ToIntPanic(p v1.Protocol) uint8 {
	pn, err := ProtoV1ToInt(p)
	if err != nil {
		panic(err)
	}
	return pn
}

func (s *Syncer) newSvcID() uint32 {
	// TODO we may run out of IDs unless we restart to recycle
	id := s.nextSvcID
	s.nextSvcID++
	return id
}

func (s *Syncer) matchBpfSvc(bpfSvc nat.FrontendKeyInterface, k8sSvc k8sp.ServicePortName, k8sInfo k8sp.ServicePort) *svcKey {
	matchNP := func() *svcKey {
		if bpfSvc.Port() == uint16(k8sInfo.NodePort()) {
			for _, nip := range s.nodePortIPs {
				if bpfSvc.Addr().Equal(nip) {
					skey := &svcKey{
						sname: k8sSvc,
						extra: getSvcKeyExtra(svcTypeNodePort, nip),
					}
					if log.GetLevel() >= log.DebugLevel {
						log.Debugf("resolved %s as %s", bpfSvc, skey)
					}
					return skey
				}
			}
		}

		return nil
	}

	if bpfSvc.Port() != uint16(k8sInfo.Port()) {
		if sk := matchNP(); sk != nil {
			return sk
		}
		return nil
	}
	matchLBSrcIP := func() bool {
		// External IP with zero Src CIDR is a valid entry and should not be considered
		// as stale
		if bpfSvc.SrcCIDR() == nat.ZeroCIDR {
			return true
		}
		// If the service does not have any source address range, treat all the entries with
		// src cidr as stale.
		if len(k8sInfo.LoadBalancerSourceRanges()) == 0 {
			return false
		}
		// If the service does have source range specified, look for a match
		for _, srcip := range k8sInfo.LoadBalancerSourceRanges() {
			if srcip != nil && srcip.IP.To4() == nil {
				continue
			}
			cidr := ip.CIDRFromIPNet(srcip).(ip.V4CIDR)
			if cidr == bpfSvc.SrcCIDR() {
				return true
			}
		}
		return false
	}

	if bpfSvc.Addr().String() == k8sInfo.ClusterIP().String() {
		if bpfSvc.SrcCIDR() == nat.ZeroCIDR {
			skey := &svcKey{
				sname: k8sSvc,
			}
			if log.GetLevel() >= log.DebugLevel {
				log.Debugf("resolved %s as %s", bpfSvc, skey)
			}
			return skey
		}
	}

	for _, eip := range k8sInfo.ExternalIPs() {
		if bpfSvc.Addr().Equal(eip) {
			if matchLBSrcIP() {
				skey := &svcKey{
					sname: k8sSvc,
					extra: getSvcKeyExtra(svcTypeExternalIP, eip),
				}
				if log.GetLevel() >= log.DebugLevel {
					log.Debugf("resolved %s as %s", bpfSvc, skey)
				}
				return skey
			}
		}
	}

	for _, lbip := range k8sInfo.LoadBalancerVIPs() {
		if len(lbip) != 0 {
			if bpfSvc.Addr().Equal(lbip) {
				if matchLBSrcIP() {
					skey := &svcKey{
						sname: k8sSvc,
						extra: getSvcKeyExtra(svcTypeLoadBalancer, lbip),
					}
					log.Debugf("resolved %s as %s", bpfSvc, skey)
					return skey
				}
			}
		}
	}
	// just in case the NodePort port is the same as the Port
	if sk := matchNP(); sk != nil {
		return sk
	}

	return nil
}

func (s *Syncer) runExpandNPFixup(misses []*expandMiss) {
	if len(misses) == 0 {
		return
	}
	expFixupStop := make(chan struct{})
	if s.expFixupStop != nil {
		log.Error("BUG: About to start node port fixup goroutine but one already seems to be running. " +
			"Stopping the old one.")
		close(s.expFixupStop)
	}
	s.expFixupStop = expFixupStop
	s.expFixupWg.Add(1)

	// start the fixer routine and exit
	go func() {
		log.Debug("fixer started")
		defer s.expFixupWg.Done()
		defer log.Debug("fixer exited")
		s.mapsLck.Lock()
		defer s.mapsLck.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// monitor if we should stop and if so, cancel any work
		go func() {
			select {
			case <-s.stop:
				cancel()
			case <-expFixupStop:
				cancel()
			case <-ctx.Done():
				// do nothing, we exited, work is done, just quit
			}
		}()

		for {
			log.Debugf("%d misses unresolved", len(misses))

			// We do one pass rightaway since we cannot know whether there
			// was an update or not before we got here
			s.rt.WaitAfter(ctx, func(lookup func(addr ip.Addr) (routes.ValueInterface, bool)) bool {
				log.Debug("Woke up")
				missesChanged := false
				var again []*expandMiss
				for _, m := range misses {
					if _, miss := s.expandNodePorts(m.sname, m.sinfo, m.eps, m.nport, lookup); miss != nil {
						again = append(again, miss)
						if !reflect.DeepEqual(m.eps, miss.eps) {
							missesChanged = true
						}
					} else {
						missesChanged = true
					}
				}

				if missesChanged && s.triggerFn != nil {
					log.Debug("Triggering a sync...")
					s.triggerFn()
				} else {
					log.Debug("Not triggering sync")
				}
				misses = again

				return len(misses) == 0 // block or not block
			})

			if len(misses) == 0 || ctx.Err() != nil {
				return
			}
		}
	}()
}

func (s *Syncer) SetTriggerFn(f func()) {
	s.triggerFn = f
}

func (s *Syncer) StopExpandNPFixup() {
	// If there was an error before we started ExpandNPFixup, there is nothing to stop
	if s.expFixupStop != nil {
		close(s.expFixupStop)
		s.expFixupWg.Wait()
		s.expFixupStop = nil
	}
}

// Stop stops the syncer
func (s *Syncer) Stop() {
	s.stopOnce.Do(func() {
		log.Info("Syncer stopping")
		close(s.stop)
		s.expFixupWg.Wait()
		log.Info("Syncer stopped")
	})
}

func (s *Syncer) cleanupSticky() error {
	debug := log.GetLevel() >= log.DebugLevel
	_ = debug // Work around linter false-positive.

	now := time.Duration(bpf.KTimeNanos())

	err := s.bpfAff.Iter(func(k, v []byte) maps.IteratorAction {
		key := s.affinityKeyFromBytes(k)
		val := s.affinityValueFromBytes(v)

		fend, ok := s.stickySvcs[key.FrontendAffinityKey()]
		if !ok {
			if debug {
				log.Debugf("cleaning affinity %v:%v - no such a service", key, val)
			}
			return maps.IterDelete
		}

		if _, ok := s.stickyEps[fend.id][val.Backend()]; !ok {
			if debug {
				log.Debugf("cleaning affinity %v:%v - no such a backend", key, val)
			}
			return maps.IterDelete
		}

		if now-val.Timestamp() > fend.timeo {
			if debug {
				log.Debugf("cleaning affinity %v:%v - expired", key, val)
			}
			return maps.IterDelete
		}
		if debug {
			log.Debugf("cleaning affinity %v:%v - keeping", key, val)
		}
		return maps.IterNone
	})
	if err != nil {
		return fmt.Errorf("NAT affinity map iterator failed: %s", err)
	}
	return nil
}

func (s *Syncer) HasSynced() bool {
	return s.synced
}

// ConntrackFrontendHasBackend returns true if the given front-backend pair exists
func (s *Syncer) ConntrackFrontendHasBackend(ip net.IP, port uint16,
	backendIP net.IP, backendPort uint16, proto uint8) (ret bool) {

	if log.GetLevel() >= log.DebugLevel {
		defer func() {
			log.WithField("ret", ret).Debug("ConntrackFrontendHasBackend")
		}()
	}

	id, ok := s.conntrackGetSvcID(ip, port, proto)
	if !ok {
		return false
	}

	backends := s.activeEpsMap[id]
	if backends == nil {
		return false
	}

	_, ok = backends[ipPort{backendIP.String(), int(backendPort)}]

	return ok
}

// ConntrackDestIsService return true if the given ip:port:proto is a known service
func (s *Syncer) ConntrackDestIsService(ip net.IP, port uint16, proto uint8) bool {
	_, ok := s.conntrackGetSvcID(ip, port, proto)
	log.WithFields(log.Fields{"IP": ip, "port": int(port), "proto": int(proto)}).Debugf("ConntrackDestIsService %t", ok)

	return ok
}

func (s *Syncer) conntrackGetSvcID(ip net.IP, port uint16, proto uint8) (uint32, bool) {
	id, ok := s.activeSvcsMap[ipPortProto{ipPort{ip.String(), int(port)}, proto}]
	if !ok {
		// Double check if it is a nodeport as if we are on the node that has
		// the backing pod for a nodeport and the nodeport was forwarded here,
		// the frontend is different.
		npIP := podNPIPStr
		if s.ipFamily == 6 {
			npIP = podNPIPV6Str
		}
		id, ok = s.activeSvcsMap[ipPortProto{ipPort{npIP, int(port)}, proto}]
		if !ok {
			return 0, false
		}
	}

	return id, ok
}

// ConntrackScanStart excludes Apply from running and builds the active maps for
// ConntrackFrontendHasBackend
func (s *Syncer) ConntrackScanStart() {
	log.Debug("ConntrackScanStart")
	s.mapsLck.Lock()

	s.activeSvcsMap = make(map[ipPortProto]uint32)
	s.activeEpsMap = make(map[uint32]map[ipPort]struct{})

	// build active maps for conntrack cleaning
	for skey, sinfo := range s.newSvcMap {
		if sinfo.count == 0 {
			continue
		}

		if isSvcKeyDerived(skey) {
			s.addActiveEps(sinfo.id, sinfo.svc, nil)
		} else {
			s.addActiveEps(sinfo.id, sinfo.svc, s.newEpsMap[skey.sname])
		}
	}
}

// ConntrackScanEnd enables Apply and frees active maps
func (s *Syncer) ConntrackScanEnd() {
	// free the maps when the iteration is complete
	s.activeSvcsMap = nil
	s.activeEpsMap = nil
	s.mapsLck.Unlock()
	log.Debug("ConntrackScanEnd")
}

func serviceInfoFromK8sServicePort(sport k8sp.ServicePort) *serviceInfo {
	sinfo := new(serviceInfo)

	// create a shallow copy
	sinfo.clusterIP = sport.ClusterIP()
	sinfo.port = sport.Port()
	sinfo.protocol = sport.Protocol()
	sinfo.nodePort = sport.NodePort()
	sinfo.sessionAffinityType = sport.SessionAffinityType()
	sinfo.stickyMaxAgeSeconds = sport.StickyMaxAgeSeconds()
	sinfo.externalIPs = sport.ExternalIPs()
	sinfo.loadBalancerVIPs = sport.LoadBalancerVIPs()
	sinfo.loadBalancerSourceRanges = sport.LoadBalancerSourceRanges()
	sinfo.healthCheckNodePort = sport.HealthCheckNodePort()
	sinfo.nodeLocalExternal = sport.ExternalPolicyLocal()
	sinfo.nodeLocalInternal = sport.InternalPolicyLocal()
	sinfo.hintsAnnotation = sport.HintsAnnotation()
	sinfo.servicePortAnnotations = sport.(*servicePort).servicePortAnnotations

	return sinfo
}

type serviceInfo struct {
	clusterIP                net.IP
	port                     int
	protocol                 v1.Protocol
	nodePort                 int
	sessionAffinityType      v1.ServiceAffinity
	stickyMaxAgeSeconds      int
	externalIPs              []net.IP
	loadBalancerSourceRanges []*net.IPNet
	loadBalancerVIPs         []net.IP
	healthCheckNodePort      int
	nodeLocalExternal        bool
	nodeLocalInternal        bool
	hintsAnnotation          string

	servicePortAnnotations
}

// ExternallyAccessible returns true if the service port is reachable via something
// other than ClusterIP (NodePort/ExternalIP/LoadBalancer)
func (info *serviceInfo) ExternallyAccessible() bool {
	return info.NodePort() != 0 || len(info.LoadBalancerVIPs()) != 0 || len(info.ExternalIPs()) != 0
}

// UsesClusterEndpoints returns true if the service port ever sends traffic to
// endpoints based on "Cluster" traffic policy
func (info *serviceInfo) UsesClusterEndpoints() bool {
	// The service port uses Cluster endpoints if the internal traffic policy is "Cluster",
	// or if it accepts external traffic at all. (Even if the external traffic policy is
	// "Local", we need Cluster endpoints to implement short circuiting.)
	return !info.InternalPolicyLocal() || info.ExternallyAccessible()
}

// UsesLocalEndpoints returns true if the service port ever sends traffic to
// endpoints based on "Local" traffic policy
func (info *serviceInfo) UsesLocalEndpoints() bool {
	return info.InternalPolicyLocal() || (info.ExternalPolicyLocal() && info.ExternallyAccessible())
}

// String is part of ServicePort interface.
func (info *serviceInfo) String() string {
	return fmt.Sprintf("%s:%d/%s", info.clusterIP, info.port, info.protocol)
}

// ClusterIP is part of ServicePort interface.
func (info *serviceInfo) ClusterIP() net.IP {
	return info.clusterIP
}

// Port is part of ServicePort interface.
func (info *serviceInfo) Port() int {
	return info.port
}

// SessionAffinityType is part of the ServicePort interface.
func (info *serviceInfo) SessionAffinityType() v1.ServiceAffinity {
	return info.sessionAffinityType
}

// StickyMaxAgeSeconds is part of the ServicePort interface
func (info *serviceInfo) StickyMaxAgeSeconds() int {
	return info.stickyMaxAgeSeconds
}

// Protocol is part of ServicePort interface.
func (info *serviceInfo) Protocol() v1.Protocol {
	return info.protocol
}

// LoadBalancerSourceRanges is part of ServicePort interface
func (info *serviceInfo) LoadBalancerSourceRanges() []*net.IPNet {
	return info.loadBalancerSourceRanges
}

// HealthCheckNodePort is part of ServicePort interface.
func (info *serviceInfo) HealthCheckNodePort() int {
	return info.healthCheckNodePort
}

// NodePort is part of the ServicePort interface.
func (info *serviceInfo) NodePort() int {
	return info.nodePort
}

// ExternalIPs is part of ServicePort interface.
func (info *serviceInfo) ExternalIPs() []net.IP {
	return info.externalIPs
}

// LoadBalancerIPs is part of ServicePort interface.
func (info *serviceInfo) LoadBalancerVIPs() []net.IP {
	return info.loadBalancerVIPs
}

// ExternalPolicyLocal returns if a service has only node local endpoints for external traffic.
func (info *serviceInfo) ExternalPolicyLocal() bool {
	return info.nodeLocalExternal
}

// InternalPolicyLocal returns if a service has only node local endpoints for internal traffic.
func (info *serviceInfo) InternalPolicyLocal() bool {
	return info.nodeLocalInternal
}

// HintsAnnotation is part of ServicePort interface.
func (info *serviceInfo) HintsAnnotation() string {
	return info.hintsAnnotation
}

// K8sServicePortOption defines options for NewK8sServicePort
type K8sServicePortOption func(interface{})

// NewK8sServicePort creates a new k8s ServicePort
func NewK8sServicePort(clusterIP net.IP, port int, proto v1.Protocol,
	opts ...K8sServicePortOption) k8sp.ServicePort {

	x := &servicePort{
		ServicePort: &serviceInfo{
			clusterIP: clusterIP,
			port:      port,
			protocol:  proto,
		},
	}

	for _, o := range opts {
		o(x)
	}
	return x
}

// ServicePortEqual compares if two k8sp.ServicePort are equal, that is all of
// their methods return equal values, i.e., they may differ in implementation,
// but present themselves equally. String() is not considered as it may differ
// for debugging reasons.
func ServicePortEqual(a, b k8sp.ServicePort) bool {
	return a.ClusterIP().Equal(b.ClusterIP()) &&
		a.Port() == b.Port() &&
		a.SessionAffinityType() == b.SessionAffinityType() &&
		a.StickyMaxAgeSeconds() == b.StickyMaxAgeSeconds() &&
		cidrEqual(a.ExternalIPs(), b.ExternalIPs()) &&
		cidrEqual(a.LoadBalancerVIPs(), b.LoadBalancerVIPs()) &&
		a.Protocol() == b.Protocol() &&
		cidrEqual(a.LoadBalancerSourceRanges(), b.LoadBalancerSourceRanges()) &&
		a.HealthCheckNodePort() == b.HealthCheckNodePort() &&
		a.NodePort() == b.NodePort() &&
		a.ExternalPolicyLocal() == b.ExternalPolicyLocal() &&
		a.InternalPolicyLocal() == b.InternalPolicyLocal() &&
		a.HintsAnnotation() == b.HintsAnnotation() &&
		a.ExternallyAccessible() == b.ExternallyAccessible() &&
		a.UsesClusterEndpoints() == b.UsesClusterEndpoints() &&
		a.UsesLocalEndpoints() == b.UsesLocalEndpoints()
}

func cidrEqual[T ip.IPOrIPNet](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}

	// optimize for a common case to avoid allocating a map
	if len(a) == 1 {
		return ip.CIDRFromIPOrIPNet(a[0]) == ip.CIDRFromIPOrIPNet(b[0])
	}

	m := make(map[ip.CIDR]struct{}, len(a))
	for _, s := range a {
		cidr := ip.CIDRFromIPOrIPNet(s)
		m[cidr] = struct{}{}
	}

	for _, s := range b {
		cidr := ip.CIDRFromIPOrIPNet(s)
		if _, ok := m[cidr]; !ok {
			return false
		}
	}
	return true
}

// K8sSvcWithLoadBalancerIPs set LoadBalancerIPStrings
func K8sSvcWithLoadBalancerIPs(ips []net.IP) K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).ServicePort.(*serviceInfo).loadBalancerVIPs = ips
	}
}

// K8sSvcWithLBSourceRangeIPs sets LBSourcePortRangeIPs
func K8sSvcWithLBSourceRangeIPs(ips []*net.IPNet) K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).ServicePort.(*serviceInfo).loadBalancerSourceRanges = ips
	}
}

// K8sSvcWithExternalIPs sets ExternalIPs
func K8sSvcWithExternalIPs(ips []net.IP) K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).ServicePort.(*serviceInfo).externalIPs = ips
	}
}

// K8sSvcWithNodePort sets the nodeport
func K8sSvcWithNodePort(np int) K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).ServicePort.(*serviceInfo).nodePort = np
	}
}

// K8sSvcWithLocalOnly sets OnlyNodeLocalEndpoints=true
func K8sSvcWithLocalOnly() K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).ServicePort.(*serviceInfo).nodeLocalExternal = true
		s.(*servicePort).ServicePort.(*serviceInfo).nodeLocalInternal = true
	}
}

// K8sSvcWithStickyClientIP sets ServiceAffinityClientIP to seconds
func K8sSvcWithStickyClientIP(seconds int) K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).ServicePort.(*serviceInfo).stickyMaxAgeSeconds = seconds
		s.(*servicePort).ServicePort.(*serviceInfo).sessionAffinityType = v1.ServiceAffinityClientIP
	}
}

// K8sSvcWithHintsAnnotation sets hints annotation to service info object
func K8sSvcWithHintsAnnotation(hintsAnnotation string) K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).ServicePort.(*serviceInfo).hintsAnnotation = hintsAnnotation
	}
}

func K8sSvcWithReapTerminatingUDP() K8sServicePortOption {
	return func(s interface{}) {
		s.(*servicePort).reapTerminatingUDP = true
	}
}
