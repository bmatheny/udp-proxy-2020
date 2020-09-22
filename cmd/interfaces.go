package main

import (
	"fmt"
	"github.com/google/gopacket/pcap"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/ipv4"
	"net"
)

// var Timeout time.Duration
var Interfaces = map[string]pcap.Interface{}

func initalizeInterface(l *Listen) {
	// find our interface via libpcap
	getConfiguredInterfaces()
	if len(Interfaces[l.iface].Addresses) == 0 {
		log.Fatalf("%s is not configured")
	}

	// configure libpcap listener
	inactive, err := pcap.NewInactiveHandle(l.iface)
	if err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}
	defer inactive.CleanUp()

	// set our timeout
	err = inactive.SetTimeout(l.timeout)
	if err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}
	// Promiscuous mode on/off
	err = inactive.SetPromisc(l.promisc)
	if err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}
	// Get the entire packet
	err = inactive.SetSnapLen(9000)
	if err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}

	// activate libpcap handle
	if l.handle, err = inactive.Activate(); err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}

	// set our BPF filter
	err = l.handle.SetBPFFilter(l.filter)
	if err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}

	// just inbound packets
	err = l.handle.SetDirection(pcap.DirectionIn)
	if err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}

	log.Debugf("Opened pcap handle on %s", l.iface)
	var u net.PacketConn = nil
	var listen string

	// create the raw socket to send UDP messages
	for _, ip := range Interfaces[l.iface].Addresses {
		// first, figure out out IPv4 address
		if net.IP.To4(ip.IP) == nil {
			continue
		}
		log.Debugf("%s: %s", l.iface, ip.IP.String())

		// create our ip:udp socket
		listen = fmt.Sprintf("%s", ip.IP.String())
		u, err = net.ListenPacket("ip:udp", listen)
		if err != nil {
			log.Fatalf("%s: %s", l.iface, err)
		}
		log.Debugf("%s: listening on %s", l.iface, listen)
		defer u.Close()
		break
	}

	// make sure we create our ip:udp socket
	if u == nil {
		log.Fatalf("%s: Unable to figure out where to listen for UDP", l.iface)
	}

	// use that ip:udp socket to create a new raw socket
	p := ipv4.NewPacketConn(u)
	defer p.Close()

	if l.raw, err = ipv4.NewRawConn(u); err != nil {
		log.Fatalf("%s: %s", l.iface, err)
	}
	log.Debugf("Opened raw socket on %s: %s", l.iface, p.LocalAddr().String())
}

// Uses libpcap to get a list of configured interfaces
// and populate the Interfaces.
func getConfiguredInterfaces() {
	if len(Interfaces) > 0 {
		return
	}
	ifs, err := pcap.FindAllDevs()
	if err != nil {
		log.Fatal(err)
	}
	for _, i := range ifs {
		if len(i.Addresses) == 0 {
			continue
		}
		Interfaces[i.Name] = i
	}
}

// Print out a list of all the interfaces that libpcap sees
func listInterfaces() {
	getConfiguredInterfaces()
	for k, v := range Interfaces {
		fmt.Printf("Interface: %s\n", k)
		for _, a := range v.Addresses {
			ones, _ := a.Netmask.Size()
			if a.Broadaddr != nil {
				fmt.Printf("\t- IP: %s/%d  Broadaddr: %s\n",
					a.IP.String(), ones, a.Broadaddr.String())
			} else if a.P2P != nil {
				fmt.Printf("\t- IP: %s/%d  PointToPoint: %s\n",
					a.IP.String(), ones, a.P2P.String())
			} else {
				fmt.Printf("\t- IP: %s/%d\n", a.IP.String(), ones)
			}
		}
		fmt.Printf("\n")
	}
}
