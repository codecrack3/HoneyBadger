/*
 *    service.go - HoneyBadger core library for detecting TCP attacks
 *    such as handshake-hijack, segment veto and sloppy injection.
 *
 *    Copyright (C) 2014  David Stainton
 *
 *    This program is free software: you can redistribute it and/or modify
 *    it under the terms of the GNU General Public License as published by
 *    the Free Software Foundation, either version 3 of the License, or
 *    (at your option) any later version.
 *
 *    This program is distributed in the hope that it will be useful,
 *    but WITHOUT ANY WARRANTY; without even the implied warranty of
 *    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *    GNU General Public License for more details.
 *
 *    You should have received a copy of the GNU General Public License
 *    along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package HoneyBadger

import (
	"code.google.com/p/gopacket"
	"code.google.com/p/gopacket/layers"
	"code.google.com/p/gopacket/pcap"
	"io"
	"log"
	"time"
)

const timeout time.Duration = time.Minute * 5 // XXX timeout connections after 5 minutes

// InquisitorOptions are user set parameters for specifying the
// details of how to proceed with honey_bager's TCP connection monitoring.
// More parameters should soon be added here!
type InquisitorOptions struct {
	Interface    string
	Filename     string
	WireDuration time.Duration
	Filter       string
	LogDir       string
	Snaplen      int
	PacketLog    bool
}

// Inquisitor sets up the connection pool and is an abstraction layer for dealing
// with incoming packets weather they be from a pcap file or directly off the wire.
type Inquisitor struct {
	InquisitorOptions
	stopChan     chan bool
	connPool     *ConnectionPool
	handle       *pcap.Handle
	AttackLogger AttackLogger
}

// NewInquisitor creates a new Inquisitor struct
func NewInquisitor(iface string, wireDuration time.Duration, filter string, snaplen int, logDir string, packetLog bool) *Inquisitor {
	i := Inquisitor{
		InquisitorOptions: InquisitorOptions{
			Interface:    iface,
			WireDuration: wireDuration,
			Filter:       filter,
			Snaplen:      snaplen,
			LogDir:       logDir,
			PacketLog:    packetLog,
		},
		connPool:     NewConnectionPool(),
		stopChan:     make(chan bool),
		AttackLogger: NewAttackJsonLogger(logDir),
	}
	return &i
}

// Stop... stops the TCP attack inquisition!
func (i *Inquisitor) Stop() {
	i.stopChan <- true
	i.AttackLogger.Stop()
	i.handle.Close()
}

// Start... starts the TCP attack inquisition!
func (i *Inquisitor) Start() {
	i.AttackLogger.Start()
	go i.receivePackets()
}

func (i *Inquisitor) receivePackets() {
	var err error
	var eth layers.Ethernet
	var ip layers.IPv4
	var tcp layers.TCP
	var payload gopacket.Payload
	var conn *Connection

	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip, &tcp, &payload)
	decoded := make([]gopacket.LayerType, 0, 4)

	if i.Filename != "" {
		log.Printf("Reading from pcap dump %q", i.Filename)
		i.handle, err = pcap.OpenOffline(i.Filename)
	} else {
		log.Printf("Starting capture on interface %q", i.Interface)
		i.handle, err = pcap.OpenLive(i.Interface, int32(i.Snaplen), true, i.WireDuration)
	}
	if err != nil {
		log.Fatal(err)
	}
	if err = i.handle.SetBPFFilter(i.Filter); err != nil {
		log.Fatal(err)
	}

	closeConnectionChan := make(chan CloseRequest)

	// XXX
	ticker := time.Tick(timeout)
	var lastTimestamp time.Time
	for {
		select {
		case <-i.stopChan:
			i.connPool.CloseAllConnections()
			return
		case <-ticker:
			if !lastTimestamp.IsZero() {
				log.Printf("lastTimestamp is %s\n", lastTimestamp)
				lastTimestamp = lastTimestamp.Add(timeout)
				closed := i.connPool.CloseOlderThan(lastTimestamp)
				if closed != 0 {
					log.Printf("timeout closed %d connections\n", closed)
				}
			}
		case closeRequest := <-closeConnectionChan:
			i.connPool.Delete(*closeRequest.Flow)
			closeRequest.CloseReadyChan <- true
		default:
			rawPacket, captureInfo, err := i.handle.ReadPacketData()
			if err == io.EOF {
				log.Print("ReadPacketData got EOF\n")
				i.Stop()
				return
			}
			if err != nil {
				continue
			}
			newPayload := new(gopacket.Payload)
			payload = *newPayload
			err = parser.DecodeLayers(rawPacket, &decoded)
			if err != nil {
				continue
			}
			flow := NewTcpIpFlowFromFlows(ip.NetworkFlow(), tcp.TransportFlow())
			packetManifest := PacketManifest{
				Timestamp: captureInfo.Timestamp,
				Flow:      flow,
				RawPacket: rawPacket,
				IP:        ip,
				TCP:       tcp,
				Payload:   payload,
			}
			if i.connPool.Has(flow) {
				conn, err = i.connPool.Get(flow)
				if err != nil {
					panic(err) // wtf
				}
			} else {
				conn = NewConnection(closeConnectionChan)
				conn.AttackLogger = i.AttackLogger
				if i.PacketLog {
					conn.PacketLogger = NewPcapLogger(i.LogDir, flow)
					conn.PacketLogger.Start()
				}
				i.connPool.Put(flow, conn)
			}
			conn.receivePacket(&packetManifest)
			lastTimestamp = captureInfo.Timestamp
		}
	}
}
