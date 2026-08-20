package main

import (
	"net"
	"testing"
)

func TestSSDPSocketAcceptsSelectedInterface(t *testing.T) {
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
}
