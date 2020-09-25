package main

import (
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/ipv4"
	"net"
	"strings"
	"sync"
	"time"
)

const SendBufferSize = 100

// Struct containing everything for an interface
type Listen struct {
	iface   string  // interface to use
	filter  string  // bpf filter string to listen on
	ports   []int32 // port(s) we listen for packets
	ipaddr  string  // dstip we send packets to
	promisc bool    // do we enable promisc on this interface?
	handle  *pcap.Handle
	raw     *ipv4.RawConn
	timeout time.Duration
	sendpkt chan Send // channel used to recieve packets we need to send
}

// List of LayerTypes we support in sendPacket()
var validLinkTypes = []layers.LinkType{
	layers.LinkTypeLoop,
	layers.LinkTypeEthernet,
	layers.LinkTypeNull,
}

// takes the list of listen or promisc and returns a list of Listen
// which then can be initialized
func processListener(interfaces *[]string, lp []string, promisc bool, bpf_filter string, ports []int32, to time.Duration) []Listen {
	var ret = []Listen{}
	for _, i := range lp {
		s := strings.Split(i, "@")
		if len(s) != 2 {
			log.Fatalf("%s is invalid.  Expected: <interface>@<ipaddr>", i)
		}
		iface := s[0]
		ipaddr := s[1]

		iface_prefix := iface + "@"
		if stringPrefixInSlice(iface_prefix, *interfaces) {
			log.Fatalf("Can't specify the same interface (%s) multiple times", iface)
		}
		*interfaces = append(*interfaces, iface)
		new := Listen{
			iface:   iface,
			filter:  bpf_filter,
			ports:   ports,
			ipaddr:  ipaddr,
			timeout: to,
			promisc: promisc,
			handle:  nil,
			raw:     nil,
			sendpkt: make(chan Send, SendBufferSize),
		}
		ret = append(ret, new)
	}
	return ret
}

// takes list of interfaces to listen on, if we should listen promiscuously,
// the BPF filter, list of ports and timeout and returns a list of processListener
func initalizeListeners(ifaces []string, promisc bool, bpf_filter string, ports []int32, timeout time.Duration) []Listen {
	// process our promisc and listen interfaces
	var interfaces = []string{}
	var listeners []Listen
	a := processListener(&interfaces, ifaces, promisc, bpf_filter, ports, timeout)
	for _, x := range a {
		listeners = append(listeners, x)
	}
	return listeners
}

// Our goroutine for processing packets
func (l *Listen) handlePackets(s *SendPktFeed, wg *sync.WaitGroup) {
	// add ourself as a sender
	s.RegisterSender(l.sendpkt, l.iface)

	// get packets from libpcap
	packetSource := gopacket.NewPacketSource(l.handle, l.handle.LinkType())
	packets := packetSource.Packets()

	// This timer is nice for debugging
	d, _ := time.ParseDuration("5s")
	ticker := time.Tick(d)

	// loop forever and ever and ever
	for {
		select {
		case s := <-l.sendpkt: // packet arrived from another interface
			l.sendPacket(s)
		case packet := <-packets: // packet arrived on this interfaces
			// is it legit?
			if packet.NetworkLayer() == nil || packet.TransportLayer() == nil || packet.TransportLayer().LayerType() != layers.LayerTypeUDP {
				log.Warnf("%s: Invalid packet", l.iface)
				continue
			} else if errx := packet.ErrorLayer(); errx != nil {
				log.Errorf("%s: Unable to decode: %s", l.iface, errx.Error())
			}

			log.Debugf("%s: received packet and fowarding onto other interfaces", l.iface)
			s.Send(packet, l.iface, l.handle.LinkType())
		case <-ticker: // our timer
			log.Debugf("handlePackets(%s) ticker", l.iface)
		}
	}
}

// Does the heavy lifting of editing & sending the packet onwards
func (l *Listen) sendPacket(sndpkt Send) {
	var eth layers.Ethernet
	var loop layers.Loopback // BSD NULL/Loopback used for OpenVPN tunnels
	var ip4 layers.IPv4      // we only support v4
	var udp layers.UDP
	var payload gopacket.Payload
	var parser *gopacket.DecodingLayerParser

	log.Debugf("processing packet from %s on %s", sndpkt.srcif, l.iface)

	switch sndpkt.linkType {
	case layers.LinkTypeNull:
		parser = gopacket.NewDecodingLayerParser(layers.LayerTypeLoopback, &loop, &ip4, &udp, &payload)
	case layers.LinkTypeLoop:
		parser = gopacket.NewDecodingLayerParser(layers.LayerTypeLoopback, &loop, &ip4, &udp, &payload)
	case layers.LinkTypeEthernet:
		parser = gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip4, &udp, &payload)
	default:
		log.Fatalf("Unsupported source linktype: 0x%02x", sndpkt.linkType)
		return
	}

	// try decoding our packet
	decoded := []gopacket.LayerType{}
	if err := parser.DecodeLayers(sndpkt.packet.Data(), &decoded); err != nil {
		log.Warnf("Unable to decode packet from %s: %s", sndpkt.srcif, err)
		return
	}

	// packet was decoded
	found_udp := false
	found_ipv4 := false
	for _, layerType := range decoded {
		switch layerType {
		case layers.LayerTypeUDP:
			found_udp = true
		case layers.LayerTypeIPv4:
			found_ipv4 = true
		}
	}
	if !found_udp || !found_ipv4 {
		log.Warnf("Packet from %s did not contain a IPv4/UDP packet", sndpkt.srcif)
		return
	}

	var ip_options []byte
	for _, o := range ip4.Options {
		s := []byte(o.String())
		ip_options = append(ip_options, s[:]...)
	}

	// build a new IPv4 Header
	h := ipv4.Header{
		Version:  4,
		Len:      int(ip4.IHL) * 4, // expects bytes, not words like the IPv4 header uses
		TOS:      int(ip4.TOS),
		TotalLen: int(ip4.Length),
		ID:       int(ip4.Id),
		Flags:    0,
		FragOff:  int(ip4.FragOffset),
		TTL:      int(ip4.TTL), // copy, don't decrement
		Protocol: 17,
		Checksum: 0,
		Src:      ip4.SrcIP.To4(),
		Dst:      net.ParseIP(l.ipaddr).To4(),
		Options:  ip_options,
	}

	log.Debugf("header %v", h)

	// Need to tell golang what fields we want to control & the outbound interface
	err := l.raw.SetControlMessage(ipv4.FlagSrc|ipv4.FlagDst|ipv4.FlagInterface, true)
	if err != nil {
		log.Fatal(err)
	}

	//	var pktdata []byte
	var cm ipv4.ControlMessage
	if err := l.raw.WriteTo(&h, payload.Payload(), &cm); err != nil {
		log.Errorf("Unable to send packet on %s: %s", l.iface, err)
	}
}

// Returns if the provided layertype is valid
func isValidLayerType(layertype layers.LinkType) bool {
	for _, b := range validLinkTypes {
		if b == layertype {
			return true
		}
	}
	return false
}
