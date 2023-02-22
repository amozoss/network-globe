package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/oschwald/maxminddb-golang"
	"storj.io/uplink"
)

var (
	device      string = "en5"
	snapshotLen int32  = 1024
	promiscuous bool   = false
	err         error
	timeout     time.Duration = 1 * time.Second
	handle      *pcap.Handle
)

func main() {
	frontendDir := flag.String("frontend-dir", "public", "static files directory")
	hostname := flag.String("host", "0.0.0.0", "name of the host")
	port := flag.Int("port", 8000, "port")
	accessGrant := flag.String("access-grant", "", "access grant for storj")
	flag.Parse()

	ctx := context.Background()
	access, err := uplink.ParseAccess(*accessGrant)
	if err != nil {
		log.Fatalln(err)
	}
	project, err := uplink.OpenProject(ctx, access)
	if err != nil {
		log.Fatalln(err)
	}
	defer project.Close()
	project.EnsureBucket(ctx, "files")

	server := NewServer(*frontendDir, project)

	go func() {
		log.Printf("Listening on %s:%d\n", *hostname, *port)
		log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", *hostname, *port), server))
	}()
	go func() {
		server.StartBroadcasts()
	}()

	db, err := maxminddb.Open("./GeoLite2-City.mmdb")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Open device
	handle, err = pcap.OpenLive(device, snapshotLen, promiscuous, timeout)
	if err != nil {
		log.Fatal(err)
	}
	defer handle.Close()
	//	filter := "tcp"
	//	filter := "tcp[13] & 2!=0"
	filter := "tcp[tcpflags] & (tcp-syn) != 0"
	err = handle.SetBPFFilter(filter)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Only capturing TCP packets.")
	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	for packet := range packetSource.Packets() {
		//printPacketInfo(packet)
		//printPacketInfo2(packet)
		ip, _, rec, err := ipToCoord(db, packet)
		if err != nil {
			//log.Println(err)
			continue
		}
		if ip != nil && rec != nil {
			//msg := fmt.Sprintf("%15s -> %-15s %20s %f,%f\n", ip.SrcIP, ip.DstIP, rec.Country.Names.En, rec.Location.Latitude, rec.Location.Longitude)
			//log.Println(msg)
			server.Queue(&Message{
				Src:  LatLng{39.781932, -104.970578},
				Dst:  LatLng{rec.Location.Latitude, rec.Location.Longitude},
				Name: rec.Country.Names.En,
			})
		}
	}
}

// map[continent:map[code:NA geoname_id:6255149 names:map[de:Nordamerika en:North America es:Norteamérica fr:Amérique du Nord ja:北アメリカ pt-BR:América do Norte ru:Северная Америка zh-CN:北美洲]]
//  country:map[geoname_id:6252001 iso_code:US names:map[de:USA en:United States es:Estados Unidos fr:États-Unis ja:アメリカ合衆国 pt-BR:Estados Unidos ru:США zh-CN:美国]]
//  location:map[accuracy_radius:1000 latitude:37.751 longitude:-97.822 time_zone:America/Chicago]
//  registered_country:map[geoname_id:6252001 iso_code:US names:map[de:USA en:United States es:Estados Unidos fr:États-Unis ja:アメリカ合衆国 pt-BR:Estados Unidos ru:США zh-CN:美国]]]
type record struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
		Names   struct {
			En string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"country"`
	Location struct {
		ISOCode   int     `maxminddb:"accuracy_radius"`
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

func ipToCoord(db *maxminddb.Reader, packet gopacket.Packet) (ip *layers.IPv4, tcp *layers.TCP, rec *record, err error) {
	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	if ipLayer != nil {
		ip, _ := ipLayer.(*layers.IPv4)
		tcpLayer := packet.Layer(layers.LayerTypeTCP)
		if tcpLayer != nil {
			tcp, _ := tcpLayer.(*layers.TCP)
			if tcp.SYN && tcp.ACK {
				err = db.Lookup(ip.SrcIP, &rec)
				if err != nil {
					log.Fatal(err)
				}
				return ip, tcp, rec, nil
			}
		}
	}
	return nil, nil, nil, errors.New("not found")
}

func printIpToCoord(db *maxminddb.Reader, packet gopacket.Packet) {
	ip, _, record, err := ipToCoord(db, packet)
	if err != nil {
		return
	}
	//fmt.Printf("%15s -> %-15s %s\n", ip.SrcIP, ip.DstIP, record.Country.ISOCode)
	fmt.Printf("%15s -> %-15s %20s %f,%f\n", ip.SrcIP, ip.DstIP, record.Country.Names.En, record.Location.Latitude, record.Location.Longitude)
}

func printPacketInfo2(packet gopacket.Packet) {
	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	if ipLayer != nil {
		ip, _ := ipLayer.(*layers.IPv4)
		//fmt.Println("IPv4: ", ip.SrcIP, "->", ip.DstIP)
		tcpLayer := packet.Layer(layers.LayerTypeTCP)
		if tcpLayer != nil {
			tcp, _ := tcpLayer.(*layers.TCP)
			fmt.Printf("%15s -> %-15s syn: %t ack: %t\n", ip.SrcIP, ip.DstIP, tcp.SYN, tcp.ACK)
		}
	}
}

func printPacketInfo(packet gopacket.Packet) {
	// Iterate over all layers, printing out each layer type
	fmt.Println("All packet layers:")
	for _, layer := range packet.Layers() {
		fmt.Println("- ", layer.LayerType())
	}
	// Let's see if the packet is an ethernet packet
	ethernetLayer := packet.Layer(layers.LayerTypeEthernet)
	if ethernetLayer != nil {
		fmt.Println("---Ethernet layer---")
		ethernetPacket, _ := ethernetLayer.(*layers.Ethernet)
		fmt.Println("Source MAC: ", ethernetPacket.SrcMAC)
		fmt.Println("Destination MAC: ", ethernetPacket.DstMAC)
		// Ethernet type is typically IPv4 but could be ARP or other
		fmt.Println("Ethernet type: ", ethernetPacket.EthernetType)
	}

	// Let's see if the packet is IP (even though the ether type told us)
	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	if ipLayer != nil {
		fmt.Println("---IPv4 layer---")
		ip, _ := ipLayer.(*layers.IPv4)

		// IP layer variables:
		// Version (Either 4 or 6)
		// IHL (IP Header Length in 32-bit words)
		// TOS, Length, Id, Flags, FragOffset, TTL, Protocol (TCP?),
		// Checksum, SrcIP, DstIP
		fmt.Println("IPv4: ", ip.SrcIP, "->", ip.DstIP)
		fmt.Println("Protocol: ", ip.Protocol)
	}

	// Let's see if the packet is TCP
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if tcpLayer != nil {
		fmt.Println("---TCP layer---")
		tcp, _ := tcpLayer.(*layers.TCP)

		// TCP layer variables:
		// SrcPort, DstPort, Seq, Ack, DataOffset, Window, Checksum, Urgent
		// Bool flags: FIN, SYN, RST, PSH, ACK, URG, ECE, CWR, NS
		fmt.Printf("From port %d to %d\n", tcp.SrcPort, tcp.DstPort)
		fmt.Println("Sequence number: ", tcp.Seq)
	}

	// When iterating through packet.Layers() above,
	// if it lists Payload layer then that is the same as
	// this applicationLayer. applicationLayer contains the payload
	applicationLayer := packet.ApplicationLayer()
	if applicationLayer != nil {
		fmt.Println("---Application layer---")
		//fmt.Printf("%s\n", applicationLayer.Payload())

		// Search for a string inside the payload
		if strings.Contains(string(applicationLayer.Payload()), "HTTP") {
			fmt.Println("HTTP found!")
		}
	}

	// Check for errors
	if err := packet.ErrorLayer(); err != nil {
		fmt.Println("Error decoding some part of the packet:", err)
	}
	fmt.Println()
	fmt.Println()
}
