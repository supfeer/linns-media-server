package main

import (
	"net"
	"testing"

	"golang.org/x/net/ipv4"
)

func TestSSDPSocketUsesSelectedInterface(t *testing.T) {
	networkInterface, localIP, err := chooseInterface("")
	if err != nil {
		t.Skip(err)
	}
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := configureSSDPSocket(socket, networkInterface); err != nil {
		t.Fatal(err)
	}

	configured, err := ipv4.NewPacketConn(socket).MulticastInterface()
	if err != nil {
		t.Fatal(err)
	}
	if configured == nil || configured.Index != networkInterface.Index {
		t.Fatalf("multicast interface = %#v, want %s (%d)", configured, networkInterface.Name, networkInterface.Index)
	}
}
