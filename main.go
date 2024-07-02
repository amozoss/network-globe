package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/oschwald/maxminddb-golang"
	"storj.io/uplink"
)

var (
	snapshotLen int32 = 1024
	promiscuous bool  = false
	err         error
	timeout     time.Duration = 1 * time.Second
	handle      *pcap.Handle
)
var dataSentToIP = make(map[string]int)
var mutex = &sync.Mutex{}

func main() {
	frontendDir := flag.String("frontend-dir", "", "Alternative static files directory")
	hostname := flag.String("host", "0.0.0.0", "name of the host")
	port := flag.Int("port", 8000, "port")

	accessGrant := flag.String("access-grant", "", "access grant for storj")
	bucket := flag.String("bucket", "network-globe", "bucket to upload to")

	mmdbPath := flag.String("geolite2-path", "./GeoLite2-City.mmdb", "path to GeoLite2-City.mmdb")

	listDevices := flag.Bool("list-devices", false, "list devices")
	// TODO Should be with a format flag and all messages should be in json
	isJson := flag.Bool("json", false, "output device list in json")

	batchSize := flag.Int("batch-size", 5, "number of detected connections to batch send to frontend")
	srcLat := flag.Float64("lat", 39.781932, "src lat - where lines start")
	srcLng := flag.Float64("lng", -104.970578, "src lng - where lines start")
	device := flag.String("device", "en0", "list devices")
	// TODO better way to do debugging
	debug := flag.Bool("debug", true, "debug messages")
	flag.Parse()

	if *listDevices {
		printDevices(*isJson)
		return
	}

	ctx := context.Background()
	var project *uplink.Project
	if *accessGrant != "" {
		access, err := uplink.ParseAccess(*accessGrant)
		if err != nil {
			log.Fatalln(err)
		}
		project, err = uplink.OpenProject(ctx, access)
		if err != nil {
			log.Fatalln(err)
		}
		defer project.Close()
		project.EnsureBucket(ctx, *bucket)
	}

	server := NewServer(*frontendDir, project, *bucket, *batchSize, *debug)

	go func() {
		log.Printf("Listening on %s:%d\n", *hostname, *port)
		log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", *hostname, *port), server))
	}()
	go func() {
		server.StartBroadcasts()
	}()

	db, err := maxminddb.Open(*mmdbPath)
	if err != nil {
		if strings.Contains(err.Error(), "no such file or directory") {
			log.Fatal(err.Error() + "\n\nDownload the free GeoLite2-City.mmdb file from:\n\n  https://dev.maxmind.com/geoip/geolite2-free-geolocation-data\n\nThen run with `--geolite2-path <path to database>`\n\n")
			return
		}
		log.Fatal(err)
	}
	defer db.Close()

	// Open device
	handle, err = pcap.OpenLive(*device, snapshotLen, promiscuous, timeout)
	if err != nil {
		// https://github.com/hortinstein/node-dash-button/issues/15
		if strings.Contains(err.Error(), "Permission denied") {
			log.Fatal(err.Error() + "\n\nTo fix run in terminal and restart:\n\n  sudo chmod +r /dev/bpf*\n\n")
			return
		}
		if strings.Contains(err.Error(), "don't have permission to capture") {
			log.Fatal(err.Error() + "\n\nMight need to run as sudo")
			return
		}
		if strings.Contains(err.Error(), "No such device exists") {
			log.Fatal(err.Error() + "\n\nDetermine your network device `:\n\n  ./network-globe --list-devices\n\nThen run with `--device <device>`\n\n")
			return
		}
		log.Fatal(err)
	}
	defer handle.Close()
	filter := "tcp"
	//	filter := "tcp[13] & 2!=0"
	//filter := "tcp[tcpflags] & (tcp-syn) != 0"
	err = handle.SetBPFFilter(filter)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Only capturing TCP packets.")
	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	myIp := getIPv4FromInterface(*device)
	for packet := range packetSource.Packets() {
		ip, _, rec, err := ipToCoord(db, packet, myIp)
		if err != nil {
			//log.Println(err)
			continue
		}
		if ip != nil && rec != nil {
			if rec.PacketSize > 900 && *debug {
				msg := fmt.Sprintf("%15s -> %-15s %d %s %20s %20s %f,%f\n", ip.SrcIP, ip.DstIP, rec.PacketSize, rec.DataDirection, myIp, rec.Country.Names.En, rec.Location.Latitude, rec.Location.Longitude)
				log.Println(msg)
			}

			src := LatLng{*srcLat, *srcLng}
			dst := LatLng{rec.Location.Latitude, rec.Location.Longitude}
			if !ip.SrcIP.Equal(myIp) {
				tmp := dst
				dst = src
				src = tmp
			}
			server.Queue(&Message{
				Src:       src,
				Dst:       dst,
				Direction: rec.DataDirection,
				Name:      rec.Country.Names.En,
			})
		}
	}
}

// map[continent:map[code:NA geoname_id:6255149 names:map[de:Nordamerika en:North America es:Norteamérica fr:Amérique du Nord ja:北アメリカ pt-BR:América do Norte ru:Северная Америка zh-CN:北美洲]]
//
//	country:map[geoname_id:6252001 iso_code:US names:map[de:USA en:United States es:Estados Unidos fr:États-Unis ja:アメリカ合衆国 pt-BR:Estados Unidos ru:США zh-CN:美国]]
//	location:map[accuracy_radius:1000 latitude:37.751 longitude:-97.822 time_zone:America/Chicago]
//	registered_country:map[geoname_id:6252001 iso_code:US names:map[de:USA en:United States es:Estados Unidos fr:États-Unis ja:アメリカ合衆国 pt-BR:Estados Unidos ru:США zh-CN:美国]]]
type record struct {
	PacketSize    int
	DataDirection string
	Country       struct {
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

func getIPv4FromInterface(device string) net.IP {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		log.Fatal(err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		log.Fatal(err)
	}

	// Get the first IPv4 address of the interface
	for _, addr := range addrs {
		switch v := addr.(type) {
		case *net.IPNet:
			// Check if this is an IPv4 address
			if v.IP.To4() != nil {
				return v.IP
			}
		case *net.IPAddr:
			// Check if this is an IPv4 address
			if v.IP.To4() != nil {
				return v.IP
			}
		}
	}

	return nil
}

func ipToCoord(db *maxminddb.Reader, packet gopacket.Packet, myIP net.IP) (ip *layers.IPv4, tcp *layers.TCP, rec *record, err error) {
	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	if ipLayer != nil {
		ip, _ := ipLayer.(*layers.IPv4)
		tcpLayer := packet.Layer(layers.LayerTypeTCP)
		if tcpLayer != nil {
			tcp, _ := tcpLayer.(*layers.TCP)

			// Skip the TCP handshake packets
			if len(tcp.Payload) > 0 {
				ipLookup := ip.SrcIP
				rec = &record{}
				if ip.SrcIP.Equal(myIP) {
					ipLookup = ip.DstIP
					rec.DataDirection = "Upload"
				} else {
					rec.DataDirection = "Download"
				}

				err = db.Lookup(ipLookup, rec)
				if err != nil {
					log.Fatal(err)
				}

				// Determine if the packet is upload or download
				rec.PacketSize = len(tcp.Payload)

				// Update the data sent to the destination IP
				mutex.Lock()
				if rec.DataDirection == "Upload" {
					dataSentToIP[ip.DstIP.String()] += len(tcp.Payload)
				} else {
					dataSentToIP[ip.SrcIP.String()] += len(tcp.Payload)
				}
				mutex.Unlock()

				return ip, tcp, rec, nil
			}
		}
	}
	return nil, nil, nil, errors.New("not found")
}

func printDevices(isJson bool) {
	devices, err := pcap.FindAllDevs()
	if err != nil {
		log.Fatal(err)
	}

	if isJson {
		jsonData, err := json.MarshalIndent(devices, "", "  ")
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println(string(jsonData))
	} else {
		// Print device information
		fmt.Println("Devices found:")
		for _, device := range devices {
			fmt.Println("\nName: ", device.Name)
			fmt.Println("Description: ", device.Description)
			fmt.Println("Devices addresses: ", device.Description)
			for _, address := range device.Addresses {
				fmt.Println("- IP address: ", address.IP)
				fmt.Println("- Subnet mask: ", address.Netmask)
			}
		}
	}
}
