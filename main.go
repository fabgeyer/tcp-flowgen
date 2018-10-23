package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"sync"
	"time"

	pb "./messages"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/mikioh/tcp"
	"github.com/mikioh/tcpinfo"
	"github.com/mikioh/tcpopt"

	log "github.com/sirupsen/logrus"
)

const (
	maxTCPPort         = 65535
	maxReservedTCPPort = 1024
	maxRandTCPPort     = maxTCPPort - (maxReservedTCPPort + 1)
)

var (
	tcpPortRand = rand.New(rand.NewSource(time.Now().UnixNano()))
)

type FlowDescription struct {
	Id             int32
	Src            string
	Dst            string
	Duration       string
	DurationParsed time.Duration `json:"-"`
	Start          string
	StartParsed    time.Duration `json:"-"`
	CCAlg          string
	ECN            bool
	NoDelay        bool
}

type FlowDir int

const (
	RX FlowDir = iota
	TX
)

type ResKey struct {
	Fid int32
	Dir FlowDir
}

type TCPInfo struct {
	*tcpinfo.Info
	TS time.Duration
}

type config struct {
	quiet             bool
	listenPort        string
	output            string
	master            string
	KeepClientsAlive  bool
	ReadSize          int
	WriteSize         int
	TCPInfoTick       string
	TCPInfoTickParsed time.Duration
	Results           map[ResKey][]*pb.TCPInfo
	ResLock           sync.RWMutex
	Flows             []*pb.FlowParams
}

type Server struct {
	conf    *config
	isSlave bool
	rlock   sync.RWMutex
	results map[int32]*pb.Results
	mlock   sync.WaitGroup
	grpcSrv *grpc.Server
}

func serializeTCPInfo(tis []TCPInfo) []*pb.TCPInfo {
	rtis := make([]*pb.TCPInfo, 0)
	for _, i := range tis {
		rtis = append(rtis, &pb.TCPInfo{
			Ts:                float64(i.TS.Seconds()),
			Castate:           int32(i.Sys.CAState),
			State:             int32(i.State),
			RTT:               i.RTT.Seconds(),
			RTTVar:            i.RTTVar.Seconds(),
			ThruBytesAcked:    i.Sys.ThruBytesAcked,
			ThruBytesReceived: i.Sys.ThruBytesReceived,
			UnackedSegs:       uint32(i.Sys.UnackedSegs),
			LostSegs:          uint32(i.Sys.LostSegs),
			RetransSegs:       uint32(i.Sys.RetransSegs),
			ReorderedSegs:     uint32(i.Sys.ReorderedSegs),
		})
	}
	return rtis
}

// This function handles the periodic probing of the TCP socket statistics
func monitor(tc *tcp.Conn, tick time.Duration, ready chan bool, stop chan bool, ctis chan []TCPInfo) {
	var tis []TCPInfo
	var o tcpinfo.Info
	var b [256]byte

	t0 := time.Now()
	ticker := time.NewTicker(tick)
	defer func() {
		ctis <- tis
		ticker.Stop()
	}()
	ready <- true

	var t time.Time
	receivedStop := false
	for {
		select {
		case t = <-ticker.C:
			break

		case <-stop:
			receivedStop = true
			t = time.Now()
			break
		}

		i, err := tc.Option(o.Level(), o.Name(), b[:])
		if err != nil {
			log.Warn(err)
			return
		}
		tis = append(tis, TCPInfo{i.(*tcpinfo.Info), t.Sub(t0)})
		if receivedStop {
			break
		}
	}
}

func setTCPOptions(tc *tcp.Conn, in *pb.FlowParams) {
	if len(in.Ccalg) > 0 {
		if err := tc.SetOption(tcpinfo.CCAlgorithm(in.Ccalg)); err != nil {
			log.Fatal(err)
		}
	}

	if in.ECN {
		if err := tc.SetOption(tcpopt.ECN(true)); err != nil {
			log.Fatal(err)
		}
	}

	if in.NoDelay {
		if err := tc.SetOption(tcpopt.NoDelay(true)); err != nil {
			log.Fatal(err)
		}
	}
}

func (s *Server) tcp_server(c chan uint32, in *pb.FlowParams) (float64, []TCPInfo, error) {
	defer s.mlock.Done()

	var err error
	pstart := time.Millisecond * time.Duration(int64(in.Start))
	pduration := time.Millisecond * time.Duration(int64(in.Duration))

	var port uint32
	var listener net.Listener
	for i := 0; i < 64; i++ {
		port = uint32(tcpPortRand.Intn(maxRandTCPPort) + maxReservedTCPPort + 1)
		listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			break
		}
	}
	if err != nil {
		return 0., nil, err
	}
	c <- port

	conn, errAccept := listener.Accept()
	if errAccept != nil {
		log.Fatalf("handle: accept: %v", errAccept)
	}
	conn.SetDeadline(time.Now().Add(pstart + pduration + time.Second))

	tc, err := tcp.NewConn(conn)
	if err != nil {
		return 0, nil, err
	}
	setTCPOptions(tc, in)

	cm := make(chan bool)
	mready := make(chan bool)
	ctis := make(chan []TCPInfo)
	go monitor(tc, s.conf.TCPInfoTickParsed, mready, cm, ctis)
	<-mready

	rx := 0
	tstart := time.Now()
	buf := make([]byte, s.conf.ReadSize)
	for {
		sz, err := tc.Conn.Read(buf)
		rx += sz
		if err == nil {
			// Do nothing
		} else if err == io.EOF {
			break
		} else if err, ok := err.(net.Error); ok && err.Timeout() {
			break
		} else {
			log.Warnf("conn.Read: %v", err)
			break
		}
	}
	elapSec := time.Since(tstart).Seconds()

	// Close socket monitor
	close(cm)
	tis := <-ctis

	kbps := 8 * float64(rx) / elapSec / 1e3
	log.Infof("F%d: RX %.2f Mbps", in.Id, kbps/1e3)
	s.rlock.Lock()
	s.results[in.Id] = &pb.Results{
		Id:      in.Id,
		Kbps:    kbps,
		TCPInfo: serializeTCPInfo(tis),
	}
	s.rlock.Unlock()
	return kbps, tis, nil
}

func (s *Server) tcp_client(c chan bool, in *pb.FlowParams) error {
	defer s.mlock.Done()
	host := fmt.Sprintf("%s:%d", in.Dst, in.Port)
	log.Infof("F%d: Starting flow to %s", in.Id, host)

	conn, err := net.Dial("tcp", host)
	if err != nil {
		return err
	}
	defer conn.Close()

	tc, err := tcp.NewConn(conn)
	if err != nil {
		return err
	}
	setTCPOptions(tc, in)
	c <- true

	cm := make(chan bool)
	mready := make(chan bool)
	ctis := make(chan []TCPInfo)
	go monitor(tc, s.conf.TCPInfoTickParsed, mready, cm, ctis)
	<-mready

	if in.Start > 0 {
		pstart := time.Millisecond * time.Duration(int64(in.Start))
		timer := time.NewTimer(pstart)
		<-timer.C
	}

	tx := 0
	buf := make([]byte, s.conf.WriteSize)
	pduration := time.Millisecond * time.Duration(int64(in.Duration))
	if err = conn.SetWriteDeadline(time.Now().Add(pduration)); err != nil {
		log.Warn(err)
	}
	for {
		sz, err := tc.Conn.Write(buf)
		tx += sz
		if err == nil {
			// Do nothing
		} else if err, ok := err.(net.Error); ok && err.Timeout() {
			break
		} else {
			log.Warnf("conn.Write: %v", err)
			break
		}
	}

	// Close socket monitor
	close(cm)
	tis := <-ctis

	kbps := 8 * float64(tx) / pduration.Seconds() / 1e3
	log.Infof("F%d: TX %.2f Mbps", in.Id, kbps/1e3)

	s.rlock.Lock()
	s.results[in.Id] = &pb.Results{
		Id:      in.Id,
		Kbps:    kbps,
		TCPInfo: serializeTCPInfo(tis),
	}
	s.rlock.Unlock()
	return nil
}

func (s *Server) Hello(ctx context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	if !s.isSlave {
		return nil, errors.New("Not implemented for master node")
	}
	return &pb.HelloReply{}, nil
}

func (s *Server) Shutdown(ctx context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	if !s.isSlave {
		return nil, errors.New("Not implemented for master node")
	}
	log.Debug("Goodbye!")
	go s.grpcSrv.GracefulStop()
	return &pb.HelloReply{}, nil
}

func (s *Server) GetResults(ctx context.Context, in *pb.FlowParams) (*pb.Results, error) {
	if !s.isSlave {
		return nil, errors.New("Not implemented for master node")
	}
	s.mlock.Wait()
	s.rlock.RLock()
	defer s.rlock.RUnlock()
	if res, ok := s.results[in.Id]; ok {
		return res, nil
	}
	return nil, fmt.Errorf("Unknown flow with id: %d", in.Id)
}

func (s *Server) StartFlow(ctx context.Context, in *pb.FlowParams) (*pb.StartStatus, error) {
	if !s.isSlave {
		return nil, errors.New("Not implemented for master node")
	}

	s.mlock.Add(1)
	c := make(chan bool)
	go s.tcp_client(c, in)
	<-c
	return &pb.StartStatus{}, nil
}

func (s *Server) Listen(ctx context.Context, in *pb.FlowParams) (*pb.ListenStatus, error) {
	if !s.isSlave {
		return nil, errors.New("Not implemented for master node")
	}

	s.mlock.Add(1)

	// Cleanup previous results
	s.rlock.Lock()
	if _, ok := s.results[in.Id]; ok {
		delete(s.results, in.Id)
	}
	s.rlock.Unlock()

	c := make(chan uint32)
	go s.tcp_server(c, in)
	port := <-c

	log.Debugf("F%d: Listening on port %d", in.Id, port)
	return &pb.ListenStatus{Port: port}, nil
}

func startRPC(app *config, slave bool) {
	lis, err := net.Listen("tcp", app.listenPort)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterTcpFlowGenServer(s, &Server{
		isSlave: slave,
		conf:    app,
		results: make(map[int32]*pb.Results),
		grpcSrv: s,
	})
	reflection.Register(s)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}

func main() {
	app := config{
		Results: make(map[ResKey][]*pb.TCPInfo),
		Flows:   make([]*pb.FlowParams, 0),
	}

	flag.StringVar(&app.output, "output", "", "Output file for results")
	flag.StringVar(&app.listenPort, "listen", "localhost:8840", "Address to listen")
	flag.StringVar(&app.master, "master", "", "Flows description")
	flag.IntVar(&app.ReadSize, "readSize", 50000, "Read buffer size in bytes")
	flag.IntVar(&app.WriteSize, "writeSize", 50000, "Write buffer size in bytes")
	flag.StringVar(&app.TCPInfoTick, "i", "250ms", "Interval for TCP statistics collections")
	flag.BoolVar(&app.KeepClientsAlive, "keepalive", false, "Keep clients alive after measurements")
	flag.BoolVar(&app.quiet, "quiet", false, "Disable printing")
	flag.Parse()

	if app.quiet {
		log.SetOutput(ioutil.Discard)
	}

	var err error
	app.TCPInfoTickParsed, err = time.ParseDuration(app.TCPInfoTick)
	if err != nil {
		log.Fatal(err)
	}

	if len(app.master) > 0 {
		go startRPC(&app, false)
		master(&app)
	} else {
		startRPC(&app, true)
	}
}
