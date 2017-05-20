package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	etcdv3 "github.com/coreos/etcd/clientv3"
	"github.com/lytics/grid/grid.v3"
)

var (
	runApi bool
)

const timeout = 2 * time.Second

// LeaderActor is the scheduler to watch the workers but the "work"
// comes from requests
type LeaderActor struct {
	client *grid.Client
}

// Act checks for peers, ie: other processes running this code,
// in the same namespace and start the worker actor on one of them.
func (a *LeaderActor) Act(c context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	existing := make(map[string]bool)
	for {
		select {
		case <-c.Done():
			return
		case <-ticker.C:
			// Ask for current peers.
			peers, err := a.client.Query(timeout, grid.Peers)
			successOrDie(err)

			// Check for new peers.
			for _, peer := range peers {
				if existing[peer.Name()] {
					continue
				}

				// Define a worker.
				existing[peer.Name()] = true
				start := grid.NewActorStart("worker-%d", len(existing))
				start.Type = "worker"

				// On new peers start the worker.
				_, err := a.client.Request(timeout, peer.Name(), start)
				successOrDie(err)
			}
		}
	}
}

// WorkerActor started by the leader.
type WorkerActor struct {
	server *grid.Server
}

// Act says hello and then waits for the exit signal.
func (a *WorkerActor) Act(ctx context.Context) {

	name, _ := grid.ContextActorName(ctx)

	fmt.Printf("worker: %q\n", name)

	// Listen to a mailbox with the same
	// name as the actor.
	mailbox, err := grid.NewMailbox(a.server, name, 10)
	successOrDie(err)
	defer mailbox.Close()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("goodbye...")
			return
		case req := <-mailbox.C:
			switch req.Msg().(type) {
			case *Event:
				fmt.Printf("msg %+v\n", req.Msg())
				err := req.Respond(&EventResponse{Id: "123"})
				if err != nil {
					fmt.Printf("error on message response %v\n", err)
				}
			default:
				fmt.Printf("ERROR:  wrong type %#v", req.Msg())
			}
		}
	}
}

func main() {
	logger := log.New(os.Stderr, "hellogrid: ", log.LstdFlags)

	address := flag.String("address", "localhost:0", "bind address for gRPC")
	flag.BoolVar(&runApi, "api", false, "run api?")
	flag.Parse()

	grid.Register(Event{})
	grid.Register(EventResponse{})

	// Connect to etcd.
	etcd, err := etcdv3.New(etcdv3.Config{Endpoints: []string{"localhost:2379"}})
	successOrDie(err)

	// Create a grid client.
	client, err := grid.NewClient(etcd, grid.ClientCfg{Namespace: "hellogrid", Logger: logger})
	successOrDie(err)

	// Create a grid server.
	server, err := grid.NewServer(etcd, grid.ServerCfg{Namespace: "hellogrid", Logger: logger})
	successOrDie(err)

	// Define how actors are created.
	server.RegisterDef("leader", func(_ []byte) (grid.Actor, error) { return &LeaderActor{client: client}, nil })
	server.RegisterDef("worker", func(_ []byte) (grid.Actor, error) { return &WorkerActor{server: server}, nil })

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println("shutting down...")
		server.Stop()
		fmt.Println("shutdown complete")
	}()

	if runApi {
		api := NewApi(client)
		go api.Run()
	}

	lis, err := net.Listen("tcp", *address)
	successOrDie(err)

	// The "leader" actor is special, it will automatically
	// get started for you when the Serve method is called.
	// The leader is always the entry-point. Even if you
	// start this app multiple times on different port
	// numbers there will only be one leader, it's a
	// singleton.
	err = server.Serve(lis)
	successOrDie(err)
}

func successOrDie(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type apiServer struct {
	c     *grid.Client
	ctx   context.Context
	peers map[string]bool
	mu    sync.Mutex
}

func NewApi(c *grid.Client) *apiServer {
	a := &apiServer{c: c}
	a.ctx = context.Background()
	return a
}
func (m *apiServer) loadWorkers() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	//m.c.QueryWatch(ctx, filter)

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// Ask for current peers.
			peers, err := m.c.Query(timeout, grid.Peers)
			successOrDie(err)
			existing := make(map[string]bool)
			m.mu.Lock()
			for _, peer := range peers {
				existing[peer.Name()] = true
			}
			m.peers = existing
			fmt.Println("found peers ", m.peers)
			m.mu.Unlock()
		}
	}
}
func (m *apiServer) Run() {
	// Ensure we have a current list
	go m.loadWorkers()

	http.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("in /work handler")
		res, err := m.c.Request(timeout, "worker-2", &Event{User: "Aaron"})
		fmt.Printf("%#v  %v\n", res, err)
		if er, ok := res.(*EventResponse); ok {
			fmt.Fprintf(w, "Response %s\n\n", er.Id)
		} else {
			fmt.Fprintf(w, "wrong response type")
		}
	})

	log.Fatal(http.ListenAndServe(":8087", nil))
}
