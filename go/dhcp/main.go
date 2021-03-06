package main

import (
	"database/sql"
	"encoding/binary"
	"strings"
	"sync"

	"context"
	_ "expvar"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/coreos/etcd/client"
	"github.com/coreos/go-systemd/daemon"
	"github.com/davecgh/go-spew/spew"
	_ "github.com/go-sql-driver/mysql"
	"github.com/goji/httpauth"
	"github.com/gorilla/mux"
	"github.com/inverse-inc/packetfence/go/log"
	"github.com/inverse-inc/packetfence/go/pfconfigdriver"
	"github.com/inverse-inc/packetfence/go/sharedutils"
	dhcp "github.com/krolaw/dhcp4"
	"github.com/patrickmn/go-cache"
)

var DHCPConfig *Interfaces

var MySQLdatabase *sql.DB

var GlobalIpCache *cache.Cache
var GlobalMacCache *cache.Cache

var GlobalTransactionCache *cache.Cache
var GlobalTransactionLock *sync.Mutex

// Control
var ControlOut map[string]chan interface{}
var ControlIn map[string]chan interface{}

var VIP map[string]bool
var VIPIp map[string]net.IP

var ctx = context.Background()

var Capi *client.Config

var webservices pfconfigdriver.PfConfWebservices

func main() {
	ctx = log.LoggerNewContext(ctx)

	// Default http timeout
	http.DefaultClient.Timeout = 10 * time.Second

	// Initialize etcd config
	Capi = etcdInit()

	// Initialize IP cache
	GlobalIpCache = cache.New(5*time.Minute, 10*time.Minute)
	// Initialize Mac cache
	GlobalMacCache = cache.New(5*time.Minute, 10*time.Minute)

	// Initialize transaction cache
	GlobalTransactionCache = cache.New(5*time.Minute, 10*time.Minute)
	GlobalTransactionLock = &sync.Mutex{}

	// Read DB config
	pfconfigdriver.PfconfigPool.AddStruct(ctx, &pfconfigdriver.Config.PfConf.Database)
	configDatabase := pfconfigdriver.Config.PfConf.Database

	connectDB(configDatabase)

	MySQLdatabase.SetMaxIdleConns(0)
	MySQLdatabase.SetMaxOpenConns(500)

	VIP = make(map[string]bool)
	VIPIp = make(map[string]net.IP)

	go func() {
		var interfaces pfconfigdriver.ListenInts
		pfconfigdriver.FetchDecodeSocket(ctx, &interfaces)
		for {
			DHCPConfig.detectVIP(interfaces)

			time.Sleep(3 * time.Second)
		}
	}()

	// Read pfconfig
	DHCPConfig = newDHCPConfig()
	DHCPConfig.readConfig()
	pfconfigdriver.PfconfigPool.AddStruct(ctx, &pfconfigdriver.Config.PfConf.Webservices)
	webservices = pfconfigdriver.Config.PfConf.Webservices

	// Queue value
	var (
		maxQueueSize = 100
		maxWorkers   = 100
	)

	ControlIn = make(map[string]chan interface{})
	ControlOut = make(map[string]chan interface{})

	// create job channel
	jobs := make(chan job, maxQueueSize)

	// create workers
	for i := 1; i <= maxWorkers; i++ {
		go func(i int) {
			for j := range jobs {
				doWork(i, j)
			}
		}(i)
	}

	// Unicast listener
	for _, v := range DHCPConfig.intsNet {
		v := v
		// Create a channel for each interfaces
		channelIn := make(chan interface{})
		channelOut := make(chan interface{})
		ControlIn[v.Name] = channelIn
		ControlOut[v.Name] = channelOut
		for net := range v.network {
			net := net
			go func() {
				v.runUnicast(jobs, v.network[net].dhcpHandler.ip)
			}()

			// We only need one listener per ip
			break
		}
	}

	// Broadcast listener
	for _, v := range DHCPConfig.intsNet {
		v := v
		go func() {
			v.run(jobs)
		}()
	}

	// Api
	router := mux.NewRouter()
	router.HandleFunc("/api/v1/dhcp/mac/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}", handleMac2Ip).Methods("GET")
	router.HandleFunc("/api/v1/dhcp/mac/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}", handleReleaseIP).Methods("DELETE")
	router.HandleFunc("/api/v1/dhcp/ip/{ip:(?:[0-9]{1,3}.){3}(?:[0-9]{1,3})}", handleIP2Mac).Methods("GET")
	router.HandleFunc("/api/v1/dhcp/stats/{int:.*}", handleStats).Methods("GET")
	router.HandleFunc("/api/v1/dhcp/debug/{int:.*}/{role:(?:[^/]*)}", handleDebug).Methods("GET")
	router.HandleFunc("/api/v1/dhcp/initialease/{int:.*}", handleInitiaLease).Methods("GET")
	router.HandleFunc("/api/v1/dhcp/options/network/{network:(?:[0-9]{1,3}.){3}(?:[0-9]{1,3})}", handleOverrideNetworkOptions).Methods("POST")
	router.HandleFunc("/api/v1/dhcp/options/network/{network:(?:[0-9]{1,3}.){3}(?:[0-9]{1,3})}", handleRemoveNetworkOptions).Methods("DELETE")
	router.HandleFunc("/api/v1/dhcp/options/mac/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}", handleOverrideOptions).Methods("POST")
	router.HandleFunc("/api/v1/dhcp/options/mac/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}", handleRemoveOptions).Methods("DELETE")
	http.Handle("/", httpauth.SimpleBasicAuth(webservices.User, webservices.Pass)(router))

	srv := &http.Server{
		Addr:        "127.0.0.1:22222",
		IdleTimeout: 5 * time.Second,
		Handler:     router,
	}

	// Systemd
	daemon.SdNotify(false, "READY=1")

	go func() {
		interval, err := daemon.SdWatchdogEnabled(false)
		if err != nil || interval == 0 {
			return
		}
		for {
			req, err := http.NewRequest("GET", "http://127.0.0.1:22222", nil)
			req.SetBasicAuth(webservices.User, webservices.Pass)
			cli := &http.Client{}
			_, err = cli.Do(req)
			if err == nil {
				daemon.SdNotify(false, "WATCHDOG=1")
			}
			time.Sleep(interval / 3)
		}
	}()
	srv.ListenAndServe()
}

// Broadcast Listener
func (h *Interface) run(jobs chan job) {

	// Communicate with the server that run on an interface
	go func() {
		inchannel := ControlIn[h.Name]
		outchannel := ControlOut[h.Name]
		for {

			Request := <-inchannel
			var stats []Stats

			// Send back stats
			if Request.(ApiReq).Req == "stats" {
				for _, v := range h.network {
					var statistics roaring.Statistics
					statistics = v.dhcpHandler.available.Stats()
					var Options map[string]string
					Options = make(map[string]string)
					Options["optionIPAddressLeaseTime"] = v.dhcpHandler.leaseDuration.String()
					for option, value := range v.dhcpHandler.options {
						key := []byte(option.String())
						key[0] = key[0] | ('a' - 'A')
						Options[string(key)] = Tlv.Tlvlist[int(option)].Decode.String(value)
					}

					// Add network options on the fly
					x, err := decodeOptions(v.network.IP.String())
					if err {
						for key, value := range x {
							Options[key.String()] = Tlv.Tlvlist[int(key)].Decode.String(value)
						}
					}

					var Members []Node
					members := v.dhcpHandler.hwcache.Items()
					var Status string
					var Count int
					Count = 0
					for i, item := range members {
						Count++
						result := make(net.IP, 4)
						binary.BigEndian.PutUint32(result, binary.BigEndian.Uint32(v.dhcpHandler.start.To4())+uint32(item.Object.(int)))
						Members = append(Members, Node{IP: result.String(), Mac: i})
					}

					if Count == (v.dhcpHandler.leaseRange - (int(statistics.RunContainerValues) + int(statistics.ArrayContainerValues))) {
						Status = "Normal"
					} else {
						Status = "Calculated available IP " + strconv.Itoa(v.dhcpHandler.leaseRange-Count) + " is different than what we have available in the pool " + strconv.Itoa(int(statistics.RunContainerValues))
					}

					stats = append(stats, Stats{EthernetName: Request.(ApiReq).NetInterface, Net: v.network.String(), Free: int(statistics.RunContainerValues) + int(statistics.ArrayContainerValues), Category: v.dhcpHandler.role, Options: Options, Members: Members, Status: Status})
				}
				outchannel <- stats
			}
			// Update the lease
			if Request.(ApiReq).Req == "initialease" {

				for _, v := range h.network {
					initiaLease(&v.dhcpHandler)
					stats = append(stats, Stats{EthernetName: Request.(ApiReq).NetInterface, Net: v.network.String(), Category: v.dhcpHandler.role, Status: "Init Lease success"})
				}
				outchannel <- stats
			}

			// Debug
			if Request.(ApiReq).Req == "debug" {
				for _, v := range h.network {
					if Request.(ApiReq).Role == v.dhcpHandler.role {
						var statistiques roaring.Statistics
						statistiques = v.dhcpHandler.available.Stats()
						spew.Dump(v.dhcpHandler.available.Stats())
						log.LoggerWContext(ctx).Info(v.dhcpHandler.available.String())
						stats = append(stats, Stats{EthernetName: Request.(ApiReq).NetInterface, Net: v.network.String(), Free: int(statistiques.RunContainerValues) + int(statistiques.ArrayContainerValues), Category: v.dhcpHandler.role, Status: "Debug finished"})
					}
				}
				outchannel <- stats
			}
		}
	}()
	ListenAndServeIf(h.Name, h, jobs)

}

// Unicast ilistener
func (h *Interface) runUnicast(jobs chan job, ip net.IP) {

	ListenAndServeIfUnicast(h.Name, h, jobs, ip)
}

func (h *Interface) ServeDHCP(ctx context.Context, p dhcp.Packet, msgType dhcp.MessageType) (answer Answer) {

	var handler DHCPHandler
	var NetScope net.IPNet
	options := p.ParseOptions()
	answer.MAC = p.CHAddr()
	answer.SrcIP = h.Ipv4

	ctx = log.AddToLogContext(ctx, "mac", answer.MAC.String())

	// Detect the handler to use (config)
	var NodeCache *cache.Cache
	NodeCache = cache.New(3*time.Second, 5*time.Second)
	var node NodeInfo
	for _, v := range h.network {

		// Case of a l2 dhcp request
		if v.dhcpHandler.layer2 && (p.GIAddr().Equal(net.IPv4zero) || v.network.Contains(p.CIAddr())) {

			// Ip per role ?
			if v.splittednet == true {

				if x, found := NodeCache.Get(p.CHAddr().String()); found {
					node = x.(NodeInfo)
				} else {
					node = NodeInformation(p.CHAddr(), ctx)
					NodeCache.Set(p.CHAddr().String(), node, 3*time.Second)
				}

				var category string
				var nodeinfo = node
				// Undefined role then use the registration one
				if nodeinfo.Category == "" || nodeinfo.Status == "unreg" {
					category = "registration"
				} else {
					category = nodeinfo.Category
				}

				if v.dhcpHandler.role == category {
					handler = v.dhcpHandler
					NetScope = v.network
					answer.SrcIP = handler.ip
					break
				}
				continue
			} else {
				// Case we are in L3
				if !p.CIAddr().Equal(net.IPv4zero) && !v.network.Contains(p.CIAddr()) {
					continue
				}
				handler = v.dhcpHandler
				NetScope = v.network
				break
			}
		}
		// Case dhcprequest from an already assigned l3 ip address
		if p.GIAddr().Equal(net.IPv4zero) && v.network.Contains(p.CIAddr()) {
			handler = v.dhcpHandler
			NetScope = v.network
			break
		}

		if (!p.GIAddr().Equal(net.IPv4zero) && v.network.Contains(p.GIAddr())) || v.network.Contains(p.CIAddr()) {
			handler = v.dhcpHandler
			NetScope = v.network
			break
		}
	}

	if len(handler.ip) == 0 {
		return answer
	}
	// Do we have the vip ?

	if VIP[h.Name] {

		log.LoggerWContext(ctx).Debug(p.CHAddr().String() + " " + msgType.String() + " xID " + sharedutils.ByteToString(p.XId()))

		GlobalTransactionLock.Lock()
		cacheKey := p.CHAddr().String() + " " + msgType.String() + " xID " + sharedutils.ByteToString(p.XId())
		if _, found := GlobalTransactionCache.Get(cacheKey); found {
			log.LoggerWContext(ctx).Debug("Not answering to packet. Already in progress")
			GlobalTransactionLock.Unlock()
			return answer
		} else {
			GlobalTransactionCache.Set(cacheKey, 1, time.Duration(5)*time.Second)
			GlobalTransactionLock.Unlock()
		}

		prettyType := "DHCP" + strings.ToUpper(msgType.String())
		clientMac := p.CHAddr().String()
		clientHostname := string(options[dhcp.OptionHostName])

		switch msgType {

		case dhcp.Discover:
			log.LoggerWContext(ctx).Info("DHCPDISCOVER from " + clientMac + " (" + clientHostname + ")")
			var free int
			i := handler.available.Iterator()

			// Search in the cache if the mac address already get assigned
			if x, found := handler.hwcache.Get(p.CHAddr().String()); found {
				free = x.(int)
				// 5 seconds to send a request
				err := handler.hwcache.Replace(p.CHAddr().String(), free, time.Duration(5)*time.Second)
				if err != nil {
					return answer
				}
				goto reply
			}

			// Search for the next available ip in the pool
		retry:
			if i.HasNext() {
				var element uint32
				// Check if the device request a specific ip
				if p.ParseOptions()[50] != nil {
					element := uint32(binary.BigEndian.Uint32(p.ParseOptions()[50])) - uint32(binary.BigEndian.Uint32(handler.start.To4()))
					if handler.available.Contains(element) {
						// Ip is available, return OFFER with this ip address
						free = int(element)
					} else {
						// Return NAK if the ip is not available
						log.LoggerWContext(ctx).Info(p.CHAddr().String() + " Nak xID " + sharedutils.ByteToString(p.XId()))
						answer.D = dhcp.ReplyPacket(p, dhcp.NAK, handler.ip.To4(), nil, 0, nil)
						return answer
					}
				} else {
					element := i.Next()
					handler.available.Remove(element)
					free = int(element)
				}

				// Lock it
				handler.hwcache.Set(p.CHAddr().String(), free, time.Duration(5)*time.Second)
				handler.xid.Set(sharedutils.ByteToString(p.XId()), 0, time.Duration(5)*time.Second)
				// Ping the ip address
				pingreply := sharedutils.Ping(dhcp.IPAdd(handler.start, free).String(), 1)
				if pingreply {
					log.LoggerWContext(ctx).Info(p.CHAddr().String() + " Ip " + dhcp.IPAdd(handler.start, free).String() + " already in use, trying next")
					// Added back in the pool since it's not the dhcp server who gave it
					handler.hwcache.Delete(p.CHAddr().String())
					goto retry
				}
				handler.available.Remove(element)
				// 5 seconds to send a request
				handler.hwcache.Set(p.CHAddr().String(), free, time.Duration(5)*time.Second)
				handler.xid.Replace(sharedutils.ByteToString(p.XId()), 1, time.Duration(5)*time.Second)
			} else {
				log.LoggerWContext(ctx).Info(p.CHAddr().String() + " Nak No space left in the pool ")
				return answer
			}

		reply:

			answer.IP = dhcp.IPAdd(handler.start, free)
			answer.Iface = h.intNet
			// Add options on the fly
			var GlobalOptions dhcp.Options
			var options = make(map[dhcp.OptionCode][]byte)
			for key, value := range handler.options {
				if key == dhcp.OptionDomainNameServer || key == dhcp.OptionRouter {
					options[key] = ShuffleIP(value)
				} else {
					options[key] = value
				}
			}
			GlobalOptions = options
			leaseDuration := handler.leaseDuration

			// Add network options on the fly
			x, err := decodeOptions(NetScope.IP.String())
			if err {
				for key, value := range x {
					if key == dhcp.OptionIPAddressLeaseTime {
						seconds, _ := strconv.Atoi(string(value))
						leaseDuration = time.Duration(seconds) * time.Second
						continue
					}
					GlobalOptions[key] = value
				}
			}

			// Add device (mac) options on the fly
			x, err = decodeOptions(p.CHAddr().String())
			if err {
				for key, value := range x {
					if key == dhcp.OptionIPAddressLeaseTime {
						seconds, _ := strconv.Atoi(string(value))
						leaseDuration = time.Duration(seconds) * time.Second
						continue
					}
					GlobalOptions[key] = value
				}
			}
			log.LoggerWContext(ctx).Info("DHCPOFFER on " + answer.IP.String() + " to " + clientMac + " (" + clientHostname + ")")
			answer.D = dhcp.ReplyPacket(p, dhcp.Offer, handler.ip.To4(), answer.IP, leaseDuration,
				GlobalOptions.SelectOrderOrAll(options[dhcp.OptionParameterRequestList]))

			return answer

		case dhcp.Request:
			reqIP := net.IP(options[dhcp.OptionRequestedIPAddress])
			if reqIP == nil {
				reqIP = net.IP(p.CIAddr())
			}

			log.LoggerWContext(ctx).Info(prettyType + " for " + reqIP.String() + " from " + clientMac + " (" + clientHostname + ")")

			answer.IP = reqIP
			answer.Iface = h.intNet

			var Reply bool
			var Index int
			// Valid IP
			if len(reqIP) == 4 && !reqIP.Equal(net.IPv4zero) {
				// Requested IP is in the pool ?
				if leaseNum := dhcp.IPRange(handler.start, reqIP) - 1; leaseNum >= 0 && leaseNum < handler.leaseRange {
					// Requested IP is in the cache ?
					if index, found := handler.hwcache.Get(p.CHAddr().String()); found {
						// Requested IP is equal to what we have in the cache ?
						if dhcp.IPAdd(handler.start, index.(int)).Equal(reqIP) {
							Reply = true
							Index = index.(int)
							// So remove the ip from the cache
						} else {
							Reply = false
							log.LoggerWContext(ctx).Info(p.CHAddr().String() + " Asked for an IP " + reqIP.String() + " that hasnt been assigned by Offer " + dhcp.IPAdd(handler.start, index.(int)).String() + " xID " + sharedutils.ByteToString(p.XId()))
							if index, found = handler.xid.Get(string(binary.BigEndian.Uint32(p.XId()))); found {
								if index.(int) == 1 {
									handler.hwcache.Delete(p.CHAddr().String())
								}
							}
						}
					} else {
						// Not in the cache so refuse
						Reply = false
					}
				}
				if Reply {

					var GlobalOptions dhcp.Options
					var options = make(map[dhcp.OptionCode][]byte)
					for key, value := range handler.options {
						if key == dhcp.OptionDomainNameServer || key == dhcp.OptionRouter {
							options[key] = ShuffleIP(value)
						} else {
							options[key] = value
						}
					}
					GlobalOptions = options
					leaseDuration := handler.leaseDuration

					// Add network options on the fly
					x, err := decodeOptions(NetScope.IP.String())
					if err {
						for key, value := range x {
							if key == dhcp.OptionIPAddressLeaseTime {
								seconds, _ := strconv.Atoi(string(value))
								leaseDuration = time.Duration(seconds) * time.Second
								continue
							}
							GlobalOptions[key] = value
						}
					}

					// Add devices options on the fly
					x, err = decodeOptions(p.CHAddr().String())
					if err {
						for key, value := range x {
							if key == dhcp.OptionIPAddressLeaseTime {
								seconds, _ := strconv.Atoi(string(value))
								leaseDuration = time.Duration(seconds) * time.Second
								continue
							}
							GlobalOptions[key] = value
						}
					}

					answer.D = dhcp.ReplyPacket(p, dhcp.ACK, handler.ip.To4(), reqIP, leaseDuration,
						GlobalOptions.SelectOrderOrAll(options[dhcp.OptionParameterRequestList]))
					// Update Global Caches
					GlobalIpCache.Set(reqIP.String(), p.CHAddr().String(), leaseDuration+(time.Duration(15)*time.Second))
					GlobalMacCache.Set(p.CHAddr().String(), reqIP.String(), leaseDuration+(time.Duration(15)*time.Second))
					// Update the cache
					log.LoggerWContext(ctx).Info("DHCPACK on " + reqIP.String() + " to " + clientMac + " (" + clientHostname + ")")
					handler.hwcache.Set(p.CHAddr().String(), Index, leaseDuration+(time.Duration(15)*time.Second))

				} else {
					log.LoggerWContext(ctx).Info("DHCPNAK on " + reqIP.String() + " to " + clientMac)
					answer.D = dhcp.ReplyPacket(p, dhcp.NAK, handler.ip.To4(), nil, 0, nil)
				}
				return answer
			}

		case dhcp.Release, dhcp.Decline:
			reqIP := net.IP(options[dhcp.OptionRequestedIPAddress])
			if reqIP == nil {
				reqIP = net.IP(p.CIAddr())
			}

			if x, found := handler.hwcache.Get(p.CHAddr().String()); found {
				handler.available.Add(uint32(x.(int)))
				handler.hwcache.Delete(p.CHAddr().String())
			}

			log.LoggerWContext(ctx).Info(prettyType + " of " + reqIP.String() + " from " + clientMac)

			return answer
		}

		answer.Iface = h.intNet
		log.LoggerWContext(ctx).Info(p.CHAddr().String() + " Nak " + sharedutils.ByteToString(p.XId()))
		answer.D = dhcp.ReplyPacket(p, dhcp.NAK, handler.ip.To4(), nil, 0, nil)
		return answer
	}
	return answer
}
