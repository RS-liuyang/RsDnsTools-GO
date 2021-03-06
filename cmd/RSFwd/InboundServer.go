package main

import (
	"time"
	"github.com/miekg/dns"
	"sync"
	log "github.com/Sirupsen/logrus"
	"gopkg.in/fatih/pool.v2"
	"os"
	"RsDnsTools/util"
	"net"
)

type Server struct {
	Addr	string
	Soreuseport	bool
	ConnTimeout	time.Duration
	debug	bool
	contextConfig	*util.Config
	defaultFwd	*util.Fwder
}


func NewServer(addr string, cc *util.Config)(*Server, error){
	s := &Server{Addr: addr, ConnTimeout: 5*time.Second, contextConfig:cc}

	return s, nil
}

func (s *Server)Run(){

	//建立forward proxy连接池
	for i:=0; i<len(s.contextConfig.Forwarders.Forwarder);i++{
		fwd := s.contextConfig.Forwarders.Forwarder[i]
		log.Infof("%dth fwd address is %s ", i, fwd.Address)
		strConn := fwd.Address
		f := func() (net.Conn, error) { return net.Dial("udp", strConn) }
		p, err := pool.NewChannelPool(1024, 4096, f)
		if err != nil {
			println("setup conn pool failed")
		} else {
			fwd.FwdPool = p
		}
		if fwd.Default == true {
			s.defaultFwd = fwd
			log.Infof("default fwd address is %s", fwd.Address)
		}
	}

	if(s.defaultFwd == nil){
		log.Fatal("There should has a default forward dns server")
	}
	mux := dns.NewServeMux();
	mux.Handle(".", s)

	wg := new(sync.WaitGroup)
	wg.Add(2)

	log.Info("Start RSFwd on " + s.Addr)

	for _, p := range [2]string{"tcp", "udp"} {
		go func(p string) {
			err := dns.ListenAndServe(s.Addr, p, mux)
			if err != nil {
				log.Fatal("Listen "+p+" failed: ", err)
				os.Exit(1)
			}
		}(p)
	}

}

func (s *Server)GetDefaultFwdAddr()(string){
	return "8.8.8.8:53"
}

func (s *Server)Fetch(fwd string, r *dns.Msg)(*dns.Msg){
	c := new(dns.Client)
	in, _, _ := c.Exchange(r, fwd)

	return in
}

func (s *Server)FetchResult(c net.Conn, m *dns.Msg) (r *dns.Msg, err error) {
	t := time.Now()

	socketTimeout := 2

	co := &dns.Conn{Conn:c}

	co.SetDeadline(t.Add(time.Duration(socketTimeout)*time.Second))

	if err = co.WriteMsg(m); err != nil {
		return nil, err
	}

	r, err = co.ReadMsg()
	if err == nil && r.Id != m.Id {
		err = dns.ErrId
	}
	return r, err
}

// ServeDNS is the entry point for every request to the address that s
// is bound to. It acts as a multiplexer for the requests zonename as
// defined in the request so that the correct zone

func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	//格式校验
	if r == nil || len(r.Question) == 0 || r.MsgHdr.Response == true {
		return
	}

	//获取客户IP和请求域名
	clientIp, _, _ := net.SplitHostPort(w.RemoteAddr().String())
	qname := r.Question[0].Name

	log.Debugf("request ip is %s, queryname is %s", clientIp, qname)

	//进行源IP和域名匹配
	aclid := s.contextConfig.AclMap.Get(clientIp)
	nameid := s.contextConfig.DnameList.GetId(qname)

	log.Debugf("client ip belongs to %d, query name belongs to %d", aclid, nameid)

	//按照匹配结果获取要进行forward所使用的上游地址
	pfwd, ok := s.contextConfig.Forwarders.FwdMap[util.HitPoint{aclid, nameid}]

	if !ok{
		pfwd = s.defaultFwd
	}

	fwdAddr := pfwd.Address

	log.Debugf("forwarder address is %s", fwdAddr)

	//ecs部分的处理
	if pfwd.Ecs == true {
		log.Debug("forwarder support ecs")
		ecs_ip, ok := s.contextConfig.AclEcsMap[aclid]
		if ok {
			util.DelEDNSClientSubnet(r)
			util.SetEDNSClientSubnet(r, ecs_ip)
			log.Debugf("forward request with ecs: %s", ecs_ip)
		}
	}

	var result *dns.Msg

	switch w.RemoteAddr().Network() {
	case "udp", "udp4", "udp6": //udp use connect pool

		conn, err := pfwd.FwdPool.Get()

		if err != nil {
			log.Errorf("Get netconn from %s pool failed, %s", pfwd.Address, err)
			return
		} else {
			result, err = s.FetchResult(conn, r)
			if err != nil {
				if brokenConn, ok := conn.(*pool.PoolConn); ok {
					brokenConn.MarkUnusable()
					brokenConn.Close()
				}
				return
			} else {
				conn.Close()
			}
		}
	default:
		c := new(dns.Client)
		c.Net = w.RemoteAddr().Network()
		result,_,_ = c.Exchange(r, fwdAddr)

	}

	w.WriteMsg(result)
}
