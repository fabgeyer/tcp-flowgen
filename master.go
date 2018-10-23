package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"time"

	pb "./messages"
	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	log "github.com/sirupsen/logrus"
)

type Host struct {
	Mgmt string
	Host string
}
type ScenarioFile struct {
	Hosts map[string]Host
	Flows []*FlowDescription
}

func parseJsonFile(app *config, fname string) (ScenarioFile, error) {
	var sc ScenarioFile
	file, err := ioutil.ReadFile(fname)
	if err != nil {
		return sc, err
	}
	json.Unmarshal(file, &sc)

	// Check JSON data for consistency
	// Parse durations in json file
	flowids := make(map[int32]bool)
	for _, flow := range sc.Flows {
		if _, ok := flowids[flow.Id]; ok {
			return sc, fmt.Errorf("Flow with id %d already defined", flow.Id)
		}
		flowids[flow.Id] = true
		if _, ok := sc.Hosts[flow.Src]; !ok {
			return sc, fmt.Errorf("Unknown source '%s' for flow %d", flow.Src, flow.Id)
		}
		if _, ok := sc.Hosts[flow.Dst]; !ok {
			return sc, fmt.Errorf("Unknown destination '%s' for flow %d", flow.Dst, flow.Id)
		}

		flow.DurationParsed, err = time.ParseDuration(flow.Duration)
		if err != nil {
			return sc, fmt.Errorf("Error with flow %d: %v", flow.Id, err)
		}

		if len(flow.Start) == 0 {
			flow.StartParsed = 0 * time.Second
		} else {
			flow.StartParsed, err = time.ParseDuration(flow.Start)
			if err != nil {
				return sc, fmt.Errorf("Error with flow %d: %v", flow.Id, err)
			}
		}

		fp := &pb.FlowParams{
			Id:       flow.Id,
			Src:      flow.Src,
			Dst:      flow.Dst,
			Duration: uint64(flow.DurationParsed.Nanoseconds() / 1e6),
			Start:    uint64(flow.StartParsed.Nanoseconds() / 1e6),
			Ccalg:    flow.CCAlg,
		}
		app.Flows = append(app.Flows, fp)
	}
	return sc, nil
}

func start_flow(app *config, wg *sync.WaitGroup, wgready *sync.WaitGroup, clistart chan bool, flow *FlowDescription, src *pb.TcpFlowGenClient, dst *pb.TcpFlowGenClient, dstHost string) {
	defer wg.Done()
	duration_ms := uint64(flow.DurationParsed.Nanoseconds() / 1e6)
	start_ms := uint64(flow.StartParsed.Nanoseconds() / 1e6)
	params := &pb.FlowParams{
		Id:       flow.Id,
		Dst:      dstHost,
		Duration: duration_ms,
		Start:    start_ms,
		Ccalg:    flow.CCAlg,
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	defer cancel1()
	rl, err := (*dst).Listen(ctx1, params)
	wgready.Done()
	if err != nil {
		log.Warnf("Error with listen request: %v", err)
		return
	}
	params.Port = rl.Port

	// Waits for all the other servers to be ready
	<-clistart

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	_, err = (*src).StartFlow(ctx2, params)
	if err != nil {
		log.Warnf("Error with start flow request: %v", err)
		return
	}
}

func master(app *config) {
	sc, err := parseJsonFile(app, app.master)
	if err != nil {
		log.Fatalf("Error with configuration file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Ping all slaves and notify them of master address
	conns := make(map[string]*pb.TcpFlowGenClient)
	for k, v := range sc.Hosts {
		conn, err := grpc.Dial(v.Mgmt, grpc.WithInsecure())
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
		defer conn.Close()

		c := pb.NewTcpFlowGenClient(conn)
		conns[k] = &c
		_, err = c.Hello(ctx, &pb.HelloRequest{})
		if err != nil {
			log.Fatalf("Could not hello request: %v", err)
		}
	}

	// Start all flows
	var wg sync.WaitGroup
	var wgready sync.WaitGroup
	var maxDuration time.Duration
	clistart := make(chan bool)
	for _, flow := range sc.Flows {
		wg.Add(1)
		wgready.Add(1)
		go start_flow(app, &wg, &wgready, clistart, flow, conns[flow.Src], conns[flow.Dst], sc.Hosts[flow.Dst].Host)
		totalDuration := flow.StartParsed+flow.DurationParsed
		if totalDuration > maxDuration {
			maxDuration = totalDuration
		}
	}
	wgready.Wait()  // Wait for all flow to be ready to send
	close(clistart) // Signal to all goroutines that all flows are ready
	wg.Wait()       // Waits for all flows to finish

	// Waits for the flows to be finished
	log.Info("Waiting for ", maxDuration, " for flows to complete")
	tmr := time.NewTimer(maxDuration)
	<-tmr.C

	// When all flows are finished, get final results
	for _, flow := range sc.Flows {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resrx, err := (*conns[flow.Dst]).GetResults(ctx, &pb.FlowParams{
			Id: int32(flow.Id),
		})
		if err != nil {
			log.Warnf("Flow %d: Error gettint server results: %v", flow.Id, err)
		} else {
			app.Results[ResKey{flow.Id, RX}] = resrx.TCPInfo
		}
		cancel()

		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		restx, err := (*conns[flow.Src]).GetResults(ctx, &pb.FlowParams{
			Id: int32(flow.Id),
		})
		if err != nil {
			log.Warnf("Flow %d: Error gettint server results: %v", flow.Id, err)
		} else {
			app.Results[ResKey{flow.Id, TX}] = restx.TCPInfo
		}
		cancel()
	}

	if !app.KeepClientsAlive {
		// Close clients
		for k, _ := range sc.Hosts {
			wg.Add(1)
			go func(c *pb.TcpFlowGenClient) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, err = (*c).Shutdown(ctx, &pb.HelloRequest{})
				if err != nil {
					log.Warnf("Host %s: Could not shutdown request: %v", k, err)
				}
			}(conns[k])
		}
		wg.Wait()
	}

	if !app.quiet {
		for _, flow := range app.Flows {
			txres, oktx := app.Results[ResKey{flow.Id, TX}]
			rxres, okrx := app.Results[ResKey{flow.Id, RX}]
			if !oktx {
				log.Warnf("F%d: missing TX results", flow.Id)
				continue
			}
			if !okrx {
				log.Warnf("F%d: missing RX results", flow.Id)
				continue
			}
			log.Infof("F%d: TX %.2f Mbps, RX %.2f Mbps", flow.Id,
				float64(txres[len(txres)-1].ThruBytesAcked)*8/float64(txres[len(txres)-1].Ts)/1e6,
				float64(rxres[len(rxres)-1].ThruBytesReceived)*8/float64(rxres[len(rxres)-1].Ts)/1e6)
		}
	}

	if len(app.output) > 0 {
		f, err := os.Create(app.output)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()

		var w io.Writer
		if strings.HasSuffix(app.output, ".gz") {
			gw := gzip.NewWriter(f)
			defer gw.Close()
			w = gw
		} else {
			w = f
		}

		for _, flow := range app.Flows {
			txres, oktx := app.Results[ResKey{flow.Id, TX}]
			rxres, okrx := app.Results[ResKey{flow.Id, RX}]
			if !(oktx && okrx) {
				continue
			}
			fs := &pb.Flow{
				Params:    flow,
				TxTCPInfo: txres,
				RxTCPInfo: rxres,
			}

			d, err := proto.Marshal(fs)
			if err != nil {
				log.Fatal(err)
			}
			bs := make([]byte, 4)

			// Network order
			binary.BigEndian.PutUint32(bs, uint32(len(d)))
			w.Write(bs)
			w.Write(d)
		}
	}
}
