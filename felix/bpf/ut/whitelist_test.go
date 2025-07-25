// Copyright (c) 2020-2021 Tigera, Inc. All rights reserved.
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

package ut_test

import (
	"testing"

	"github.com/google/gopacket/layers"
	. "github.com/onsi/gomega"

	"github.com/projectcalico/calico/felix/bpf/conntrack"
	v3 "github.com/projectcalico/calico/felix/bpf/conntrack/v3"
	"github.com/projectcalico/calico/felix/bpf/routes"
	tcdefs "github.com/projectcalico/calico/felix/bpf/tc/defs"
	"github.com/projectcalico/calico/felix/ip"
)

// Usually a packet passes through 2 programs, HEP->WEP, WEP->HEP or WEP->WEP. These test
// make sure that both programs allow the traffic if their policies allow it.

func TestAllowFromWorkloadExitHost(t *testing.T) {
	RegisterTestingT(t)

	bpfIfaceName = "WHwl"
	defer func() { bpfIfaceName = "" }()
	defer cleanUpMaps()

	_, ipv4, l4, _, pktBytes, err := testPacketUDPDefault()
	Expect(err).NotTo(HaveOccurred())
	udp := l4.(*layers.UDP)

	resetCTMap(ctMap) // ensure it is clean

	hostIP = node1ip

	// Insert a reverse route for the source workload.
	rtKey := routes.NewKey(srcV4CIDR).AsBytes()
	rtVal := routes.NewValueWithIfIndex(routes.FlagsLocalWorkload|routes.FlagInIPAMPool, 1).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())

	ctKey := conntrack.NewKey(uint8(ipv4.Protocol),
		ipv4.SrcIP, uint16(udp.SrcPort), ipv4.DstIP, uint16(udp.DstPort))

	// Leaving workload
	skbMark = 0
	runBpfTest(t, "calico_from_workload_ep", rulesDefaultAllow, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_REDIRECT))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by WEP
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		// Not approved by HEP yet
		Expect(ctr.Data().B2A.Approved).NotTo(BeTrue())
	})

	// Leaving node 1
	expectMark(tcdefs.MarkSeen)

	runBpfTest(t, "calico_to_host_ep", nil, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_UNSPEC))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by both WEP and HEP
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		Expect(ctr.Data().B2A.Approved).To(BeTrue())
	})
}

func TestSkipIngressRedirect(t *testing.T) {
	RegisterTestingT(t)

	bpfIfaceName = "HWvwl"
	defer func() { bpfIfaceName = "" }()
	_, ipv4, l4, _, pktBytes, err := testPacketUDPDefault()
	Expect(err).NotTo(HaveOccurred())
	udp := l4.(*layers.UDP)

	ctMap := conntrack.Map()
	err = ctMap.EnsureExists()
	Expect(err).NotTo(HaveOccurred())
	resetCTMap(ctMap) // ensure it is clean
	defer resetCTMap(ctMap)

	hostIP = node1ip
	// Insert a reverse route for the source workload.
	rtKey := routes.NewKey(srcV4CIDR).AsBytes()
	rtVal := routes.NewValue(routes.FlagsRemoteWorkload | routes.FlagInIPAMPool).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())
	rtKey = routes.NewKey(dstV4CIDR).AsBytes()
	rtVal = routes.NewValueWithIfIndex(routes.FlagsLocalWorkload|routes.FlagSkipIngressRedir|routes.FlagInIPAMPool, 1).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())
	defer resetRTMap(rtMap)
	ctKey := conntrack.NewKey(uint8(ipv4.Protocol),
		ipv4.SrcIP, uint16(udp.SrcPort), ipv4.DstIP, uint16(udp.DstPort))
	skbMark = 0
	runBpfTest(t, "calico_from_host_ep", nil, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_REDIRECT))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by HEP
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		// Not approved by WEP yet
		Expect(ctr.Data().B2A.Approved).NotTo(BeTrue())
		Expect(ctr.Flags() & v3.FlagNoRedirPeer).To(Equal(v3.FlagNoRedirPeer))
	})

	// Reset route map and add reverse route from local workload with skip ingress redirect flag
	resetRTMap(rtMap)
	rtKey = routes.NewKey(srcV4CIDR).AsBytes()
	rtVal = routes.NewValueWithIfIndex(routes.FlagsLocalWorkload|routes.FlagSkipIngressRedir|routes.FlagInIPAMPool, 1).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())
	rtKey = routes.NewKey(dstV4CIDR).AsBytes()
	rtVal = routes.NewValueWithIfIndex(routes.FlagsLocalWorkload|routes.FlagInIPAMPool, 1).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())

	resetCTMap(ctMap)

	skbMark = 0
	runBpfTest(t, "calico_from_workload_ep", rulesDefaultAllow, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_REDIRECT))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by src WEP
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		// Not approved by dst WEP yet
		Expect(ctr.Data().B2A.Approved).NotTo(BeTrue())
		Expect(ctr.Flags() & v3.FlagNoRedirPeer).To(Equal(v3.FlagNoRedirPeer))
	})
}

func TestAllowEnterHostToWorkload(t *testing.T) {
	RegisterTestingT(t)

	bpfIfaceName = "HWwl"
	defer func() { bpfIfaceName = "" }()

	_, ipv4, l4, _, pktBytes, err := testPacketUDPDefault()
	Expect(err).NotTo(HaveOccurred())
	udp := l4.(*layers.UDP)

	ctMap := conntrack.Map()
	err = ctMap.EnsureExists()
	Expect(err).NotTo(HaveOccurred())
	resetCTMap(ctMap) // ensure it is clean

	hostIP = node1ip

	// Insert a reverse route for the source workload.
	rtKey := routes.NewKey(srcV4CIDR).AsBytes()
	rtVal := routes.NewValue(routes.FlagsRemoteWorkload | routes.FlagInIPAMPool).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())
	rtKey = routes.NewKey(dstV4CIDR).AsBytes()
	rtVal = routes.NewValueWithIfIndex(routes.FlagsLocalWorkload|routes.FlagInIPAMPool, 1).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())
	defer resetRTMap(rtMap)

	ctKey := conntrack.NewKey(uint8(ipv4.Protocol),
		ipv4.SrcIP, uint16(udp.SrcPort), ipv4.DstIP, uint16(udp.DstPort))

	skbMark = 0
	runBpfTest(t, "calico_from_host_ep", nil, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_REDIRECT))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by HEP
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		// Not approved by WEP yet
		Expect(ctr.Data().B2A.Approved).NotTo(BeTrue())
	})

	expectMark(tcdefs.MarkSeen)

	runBpfTest(t, "calico_to_workload_ep", rulesDefaultAllow, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_UNSPEC))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Still approved both by HEP and WEP
		Expect(ctr.Data().B2A.Approved).To(BeTrue())
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
	})
}

func TestAllowWorkloadToWorkload(t *testing.T) {
	RegisterTestingT(t)

	bpfIfaceName = "WWwl"
	defer func() { bpfIfaceName = "" }()

	_, ipv4, l4, _, pktBytes, err := testPacketUDPDefault()
	Expect(err).NotTo(HaveOccurred())
	udp := l4.(*layers.UDP)

	ctMap := conntrack.Map()
	err = ctMap.EnsureExists()
	Expect(err).NotTo(HaveOccurred())
	resetCTMap(ctMap) // ensure it is clean

	hostIP = node1ip

	// Insert a reverse route for the source workload.
	rtKey := routes.NewKey(srcV4CIDR).AsBytes()
	rtVal := routes.NewValueWithIfIndex(routes.FlagsLocalWorkload|routes.FlagInIPAMPool, 1).AsBytes()
	err = rtMap.Update(rtKey, rtVal)
	defer func() {
		err := rtMap.Delete(rtKey)
		Expect(err).NotTo(HaveOccurred())
	}()
	Expect(err).NotTo(HaveOccurred())

	ctKey := conntrack.NewKey(uint8(ipv4.Protocol),
		ipv4.SrcIP, uint16(udp.SrcPort), ipv4.DstIP, uint16(udp.DstPort))

	skbMark = 0
	runBpfTest(t, "calico_from_workload_ep", rulesDefaultAllow, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_REDIRECT))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by the first WEP (on egress from WEP)
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		// Not approved by the second WEP yet
		Expect(ctr.Data().B2A.Approved).NotTo(BeTrue())
	})

	expectMark(tcdefs.MarkSeen)

	runBpfTest(t, "calico_to_workload_ep", rulesDefaultAllow, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_UNSPEC))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by both WEPs
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		Expect(ctr.Data().B2A.Approved).To(BeTrue())
	})
}

func TestAllowFromHostExitHost(t *testing.T) {
	RegisterTestingT(t)

	bpfIfaceName = "WHhs"
	defer func() { bpfIfaceName = "" }()
	defer cleanUpMaps()

	ipHdr := ipv4Default
	ipHdr.Id = 1
	ipHdr.SrcIP = node1ip
	ipHdr.DstIP = node2ip

	_, ipv4, l4, _, pktBytes, err := testPacketV4(nil, ipHdr, nil, nil)
	Expect(err).NotTo(HaveOccurred())
	udp := l4.(*layers.UDP)

	resetCTMap(ctMap) // ensure it is clean

	hostIP = node1ip

	// Insert routes for both hosts.
	err = rtMap.Update(
		routes.NewKey(ip.CIDRFromIPNet(&node1CIDR).(ip.V4CIDR)).AsBytes(),
		routes.NewValue(routes.FlagsLocalHost).AsBytes(),
	)
	Expect(err).NotTo(HaveOccurred())
	err = rtMap.Update(
		routes.NewKey(ip.CIDRFromIPNet(&node2CIDR).(ip.V4CIDR)).AsBytes(),
		routes.NewValue(routes.FlagsRemoteHost).AsBytes(),
	)
	Expect(err).NotTo(HaveOccurred())

	ctKey := conntrack.NewKey(uint8(ipv4.Protocol),
		ipv4.SrcIP, uint16(udp.SrcPort), ipv4.DstIP, uint16(udp.DstPort))

	// Leaving node 1
	skbMark = tcdefs.MarkSeen

	runBpfTest(t, "calico_to_host_ep", nil, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_UNSPEC))

		dumpCTMap(ctMap)

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by HEP
		Expect(ctr.Data().A2B.Approved).To(BeFalse())
		Expect(ctr.Data().B2A.Approved).To(BeTrue())
	})

	// Return
	skbMark = 0
	runBpfTest(t, "calico_from_host_ep", nil, func(bpfrun bpfProgRunFn) {
		respPkt := udpResponseRaw(pktBytes)
		res, err := bpfrun(respPkt)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_REDIRECT))

		dumpCTMap(ctMap)

		ctKey := conntrack.NewKey(uint8(ipv4.Protocol),
			ipv4.SrcIP, uint16(udp.SrcPort), ipv4.DstIP, uint16(udp.DstPort))

		ct, err := conntrack.LoadMapMem(ctMap)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by HEP
		Expect(ctr.Data().A2B.Approved).To(BeFalse())
		Expect(ctr.Data().B2A.Approved).To(BeTrue())
	})
}

func TestAllowEnterHostToWorkloadV6(t *testing.T) {
	RegisterTestingT(t)

	bpfIfaceName = "HWwl"
	defer func() { bpfIfaceName = "" }()

	hop := &layers.IPv6HopByHop{}
	hop.NextHeader = layers.IPProtocolUDP

	/* from gopacket ip6_test.go */
	tlv := &layers.IPv6HopByHopOption{}
	tlv.OptionType = 0x01 //PadN
	tlv.OptionData = []byte{0x00, 0x00, 0x00, 0x00}
	hop.Options = append(hop.Options, tlv)

	_, _, l4, _, pktBytes, err := testPacketV6(nil, ipv6Default, nil, nil, hop)
	Expect(err).NotTo(HaveOccurred())
	udp := l4.(*layers.UDP)

	resetMap(ctMapV6) // ensure it is clean

	hostIP = node1ip

	// Insert a reverse route for the source workload.
	rtKey := routes.NewKeyV6(srcV6CIDR).AsBytes()
	rtVal := routes.NewValueV6(routes.FlagsRemoteWorkload | routes.FlagInIPAMPool).AsBytes()
	err = rtMapV6.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())
	rtKey = routes.NewKeyV6(dstV6CIDR).AsBytes()
	rtVal = routes.NewValueV6WithIfIndex(routes.FlagsLocalWorkload|routes.FlagInIPAMPool, 1).AsBytes()
	err = rtMapV6.Update(rtKey, rtVal)
	Expect(err).NotTo(HaveOccurred())
	defer resetRTMap(rtMapV6)

	dumpRTMapV6(rtMapV6)

	ctKey := conntrack.NewKeyV6(17, /* UDP */
		ipv6Default.SrcIP, uint16(udp.SrcPort), ipv6Default.DstIP, uint16(udp.DstPort))

	skbMark = 0
	runBpfTest(t, "calico_from_host_ep", nil, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_UNSPEC))

		ct, err := conntrack.LoadMapMemV6(ctMapV6)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Approved by HEP
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
		// Not approved by WEP yet
		Expect(ctr.Data().B2A.Approved).NotTo(BeTrue())
	}, withIPv6())

	expectMark(tcdefs.MarkSeen)

	dumpCTMapV6(ctMapV6)

	runBpfTest(t, "calico_to_workload_ep", rulesDefaultAllow, func(bpfrun bpfProgRunFn) {
		res, err := bpfrun(pktBytes)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Retval).To(Equal(resTC_ACT_UNSPEC))

		ct, err := conntrack.LoadMapMemV6(ctMapV6)
		Expect(err).NotTo(HaveOccurred())
		Expect(ct).Should(HaveKey(ctKey))

		ctr := ct[ctKey]

		// Still approved both by HEP and WEP
		Expect(ctr.Data().B2A.Approved).To(BeTrue())
		Expect(ctr.Data().A2B.Approved).To(BeTrue())
	}, withIPv6())
}
