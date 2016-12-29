package grid

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	etcdv3 "github.com/coreos/etcd/clientv3"
	"github.com/lytics/grid/grid.v3/testetcd"
)

func TestServerExample(t *testing.T) {
	etcd, cleanup := bootstrap(t)
	defer cleanup()
	errs := &testerrors{}

	client, err := NewClient(etcd, ClientCfg{Namespace: "example_grid"})
	if err != nil {
		t.Fatal(err)
	}

	e := &ExampleGrid{errs: errs, client: client}
	g, err := NewServer(etcd, ServerCfg{Namespace: "example_grid"}, e)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	abort := make(chan struct{}, 1)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-abort //shutting down...
		if len(errs.errors()) == 0 {
			g.Stop() //shutdown complete
		}
	}()

	lis, err := net.Listen("tcp", "localhost:0") //let the OS pick a port for us.
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		// After calling Serve one node within your Grid will be elected as
		// a "leader" and that grid's MakeActor() will be called with a
		// def.Type == "leader".  You can think of this as the main for the
		// grid cluster.
		err = g.Serve(lis)
		if err != nil {
			errs.append(err)
		}
	}()

	time.Sleep(2 * time.Second)
	close(abort)
	wg.Wait()

	if len(errs.errors()) > 0 {
		t.Fatalf("we got errors: %v", errs.errors())
	}

	ls := atomic.LoadInt64(&e.leadersStarted)
	le := atomic.LoadInt64(&e.leadersEnded)
	ws := atomic.LoadInt64(&e.workersStarted)
	we := atomic.LoadInt64(&e.workersEnded)

	if ls != 1 || le != 1 {
		t.Fatalf("leader's lifecycle isn't correct: leaderstarts:%d leaderstops:%d", ls, le)
	}
	if ws != 1 || we != 1 {
		t.Fatalf("leader's lifecycle isn't correct: workerstarts:%d workerstops:%d", ws, we)
	}
}

type ExampleGrid struct {
	leadersStarted int64
	leadersEnded   int64
	workersStarted int64
	workersEnded   int64

	errs   *testerrors
	client *Client
}

func (e *ExampleGrid) MakeActor(def *ActorDef) (Actor, error) {
	switch def.Type {
	case "leader":
		return &ExampleLeader{e}, nil
	case "worker":
		return &ExampleWorker{e}, nil
	}
	return nil, fmt.Errorf("unknow actor type: %v", def.Type)
}

type ExampleLeader struct{ e *ExampleGrid }

func (a *ExampleLeader) Act(c context.Context) {
	atomic.AddInt64(&(a.e.leadersStarted), 1)
	defer atomic.AddInt64(&(a.e.leadersEnded), 1)

	def := NewActorDef("worker-%d", 1)
	def.Type = "worker"

	peers, err := a.e.client.Peers(time.Second)
	if err != nil || len(peers) == 0 {
		a.e.errs.append(fmt.Errorf("failed to get list of peers:%v", err))
		return
	}

	if len(peers) != 1 {
		a.e.errs.append(fmt.Errorf("the list of peers != 1:%v", peers))
		return
	}

	_, err = a.e.client.Request(time.Second, peers[0], def)
	if err != nil || len(peers) == 0 {
		a.e.errs.append(fmt.Errorf("create worker request failed:%v", err))
		return
	}

	<-c.Done()
}

type ExampleWorker struct{ e *ExampleGrid }

func (a *ExampleWorker) Act(c context.Context) {
	atomic.AddInt64(&a.e.workersStarted, 1)
	defer atomic.AddInt64(&a.e.workersEnded, 1)
	<-c.Done()
}

func bootstrap(t *testing.T) (*etcdv3.Client, testetcd.Cleanupfn) {
	srvcfg, cleanup, err := testetcd.StartEtcd(t)
	if err != nil {
		t.Fatalf("err:%v", err)
	}

	urls := []string{}
	for _, u := range srvcfg.LCUrls {
		urls = append(urls, u.String())
	}
	cfg := etcdv3.Config{
		Endpoints: urls,
	}
	etcd, err := etcdv3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return etcd, cleanup
}

type testerrors struct {
	mu   sync.Mutex
	errs []error
}

func (e *testerrors) append(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errs = append(e.errs, err)
}

func (e *testerrors) errors() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.errs
}
