package test

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sort"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/pilosa/pilosa"
	"github.com/pilosa/pilosa/internal"
)

// NewCluster returns a cluster with n nodes and uses a mod-based hasher.
func NewCluster(n int) *pilosa.Cluster {
	path, err := ioutil.TempDir("", "pilosa-cluster-")
	if err != nil {
		panic(err)
	}

	c := pilosa.NewCluster()
	c.ReplicaN = 1
	c.Hasher = NewModHasher()
	c.Path = path
	c.Topology = pilosa.NewTopology()

	for i := 0; i < n; i++ {
		c.Nodes = append(c.Nodes, &pilosa.Node{
			URI: NewURI("http", fmt.Sprintf("host%d", i), uint16(0)),
		})
	}

	return c
}

// ModHasher represents a simple, mod-based hashing.
type ModHasher struct{}

// NewModHasher returns a new instance of ModHasher with n buckets.
func NewModHasher() *ModHasher { return &ModHasher{} }

func (*ModHasher) Hash(key uint64, n int) int { return int(key) % n }

// ConstHasher represents hash that always returns the same index.
type ConstHasher struct {
	i int
}

// NewConstHasher returns a new instance of ConstHasher that always returns i.
func NewConstHasher(i int) *ConstHasher { return &ConstHasher{i: i} }

func (h *ConstHasher) Hash(key uint64, n int) int { return h.i }

// NewURI is a test URI creator that intentionally swallows errors.
func NewURI(scheme, host string, port uint16) pilosa.URI {
	uri := pilosa.DefaultURI()
	uri.SetScheme(scheme)
	uri.SetHost(host)
	uri.SetPort(port)
	return *uri
}

func NewURIFromHostPort(host string, port uint16) pilosa.URI {
	uri := pilosa.DefaultURI()
	uri.SetHost(host)
	uri.SetPort(port)
	return *uri
}

// TestCluster represents a cluster of test nodes, each of which
// has a pilosa.Cluster.
type TestCluster struct {
	Clusters []*pilosa.Cluster

	common *commonClusterSettings

	resizeDone chan struct{}
}

type commonClusterSettings struct {
	NodeSet pilosa.NodeSet
}

func (t *TestCluster) CreateIndex(name string) error {
	for _, c := range t.Clusters {
		if _, err := c.Holder.CreateIndexIfNotExists(name, pilosa.IndexOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (t *TestCluster) CreateFrame(index, frame string, opt pilosa.FrameOptions) error {
	for _, c := range t.Clusters {
		idx, err := c.Holder.CreateIndexIfNotExists(index, pilosa.IndexOptions{})
		if err != nil {
			return err
		}
		if _, err := idx.CreateFrame(frame, opt); err != nil {
			return err
		}
	}
	return nil
}
func (t *TestCluster) SetBit(index, frame, view string, rowID, colID uint64, x *time.Time) error {
	// Determine which node should receive the SetBit.
	c0 := t.Clusters[0] // use the first node's cluster to determine slice location.
	slice := colID / pilosa.SliceWidth
	nodes := c0.FragmentNodes(index, slice)

	for _, node := range nodes {
		c := t.clusterByURI(node.URI)
		if c == nil {
			continue
		}
		f := c.Holder.Frame(index, frame)
		if f == nil {
			return fmt.Errorf("index/frame does not exist: %s/%s", index, frame)
		}
		_, err := f.SetBit(view, rowID, colID, x)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *TestCluster) clusterByURI(uri pilosa.URI) *pilosa.Cluster {
	for _, c := range t.Clusters {
		if c.URI == uri {
			return c
		}
	}
	return nil
}

// AddNode adds a node to the cluster and (potentially) starts a resize job.
func (t *TestCluster) AddNode(saveTopology bool) error {
	id := len(t.Clusters)

	c, err := t.addCluster(id, saveTopology)
	if err != nil {
		return err
	}

	// Send NodeJoin event to coordinator.
	if id > 0 {
		coord := t.Clusters[0]
		ev := &pilosa.NodeEvent{
			Event: pilosa.NodeJoin,
			URI:   c.URI,
		}

		//go coord.ReceiveEvent(ev)
		if err := coord.ReceiveEvent(ev); err != nil {
			return err
		}

		// Wait for the AddNode job to finish.
		if c.State != pilosa.ClusterStateNormal {
			t.resizeDone = make(chan struct{})
			<-t.resizeDone
		}
	}

	return nil
}

// WriteTopology writes the given topology to disk.
func (t *TestCluster) WriteTopology(path string, top *pilosa.Topology) error {
	if buf, err := proto.Marshal(top.Encode()); err != nil {
		return err
	} else if err := ioutil.WriteFile(filepath.Join(path, ".topology"), buf, 0666); err != nil {
		return err
	}
	return nil
}

func (t *TestCluster) addCluster(i int, saveTopology bool) (*pilosa.Cluster, error) {

	uri := NewURI("http", fmt.Sprintf("host%d", i), uint16(0))

	// add URI to common
	t.common.NodeSet = append(t.common.NodeSet, uri)
	sort.Sort(t.common.NodeSet)

	// create node-specific temp directory
	path, err := ioutil.TempDir("", fmt.Sprintf("pilosa-cluster-node-%d-", i))
	if err != nil {
		return nil, err
	}

	// holder
	h := pilosa.NewHolder()
	h.Path = path

	// cluster
	c := pilosa.NewCluster()
	c.ReplicaN = 1
	c.Hasher = NewModHasher()
	c.Path = path
	c.Topology = pilosa.NewTopology()
	c.Holder = h
	c.MemberSet = pilosa.NewStaticMemberSet()
	c.URI = uri
	c.Coordinator = t.common.NodeSet[0] // the first node is the coordinator
	c.Broadcaster = t

	// add nodes
	if saveTopology {
		for _, u := range t.common.NodeSet {
			c.AddNode(u)
		}
	}

	// Add this node to the TestCluster.
	t.Clusters = append(t.Clusters, c)

	return c, nil
}

// NewTestCluster returns a new instance of test.Cluster.
func NewTestCluster(n int) *TestCluster {

	tc := &TestCluster{
		common: &commonClusterSettings{},
	}

	// add clusters
	for i := 0; i < n; i++ {
		_, err := tc.addCluster(i, true)
		if err != nil {
			panic(err)
		}
	}
	return tc
}

// SetState sets the state of the cluster on each node.
func (t *TestCluster) SetState(state string) {
	for _, c := range t.Clusters {
		c.State = state
	}
}

// Open opens all clusters in the test cluster.
func (t *TestCluster) Open() error {
	for _, c := range t.Clusters {
		if err := c.Open(); err != nil {
			return err
		}
		if err := c.Holder.Open(); err != nil {
			return err
		}
		if err := c.SetNodeState(pilosa.NodeStateReady); err != nil {
			return err
		}
	}

	// Start the listener on the coordinator.
	if len(t.Clusters) == 0 {
		return nil
	}
	t.Clusters[0].ListenForJoins()

	return nil
}

// Close closes all clusters in the test cluster.
func (t *TestCluster) Close() error {
	for _, c := range t.Clusters {
		err := c.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// TestCluster implements Broadcaster interface.

// SendSync is a test implemenetation of Broadcaster SendSync method.
func (t *TestCluster) SendSync(pb proto.Message) error {
	switch obj := pb.(type) {
	case *internal.ClusterStatus:
		// Apply the send message to all nodes (except the coordinator).
		for _, c := range t.Clusters {
			c.MergeClusterStatus(obj)
		}
		if obj.State == pilosa.ClusterStateNormal && t.resizeDone != nil {
			close(t.resizeDone)
		}
	}

	return nil
}

// SendAsync is a test implemenetation of Broadcaster SendAsync method.
func (t *TestCluster) SendAsync(pb proto.Message) error {
	return nil
}

// SendTo is a test implemenetation of Broadcaster SendTo method.
func (t *TestCluster) SendTo(to *pilosa.Node, pb proto.Message) error {
	switch obj := pb.(type) {
	case *internal.ResizeInstruction:
		t.FollowResizeInstruction(obj)
	case *internal.ResizeInstructionComplete:
		coord := t.clusterByURI(to.URI)
		go coord.MarkResizeInstructionComplete(obj)
	}
	return nil
}

// FollowResizeInstruction is a version of cluster.FollowResizeInstruction used for testing.
func (t *TestCluster) FollowResizeInstruction(instr *internal.ResizeInstruction) error {

	// Prepare the return message.
	complete := &internal.ResizeInstructionComplete{
		JobID: instr.JobID,
		URI:   instr.URI,
		Error: "",
	}

	// figure out which node it was meant for, then call the operation on that cluster
	// basically need to mimic this: client.RetrieveSliceFromURI(context.Background(), src.Index, src.Frame, src.View, src.Slice, srcURI)
	instrURI := pilosa.DecodeURI(instr.URI)
	destCluster := t.clusterByURI(instrURI)

	// Sync the schema received in the resize instruction.
	if err := destCluster.Holder.ApplySchema(instr.Schema); err != nil {
		return err
	}

	for _, src := range instr.Sources {
		srcURI := pilosa.DecodeURI(src.URI)
		srcCluster := t.clusterByURI(srcURI)

		srcFragment := srcCluster.Holder.Fragment(src.Index, src.Frame, src.View, src.Slice)
		destFragment := destCluster.Holder.Fragment(src.Index, src.Frame, src.View, src.Slice)
		if destFragment == nil {
			// Create fragment on destination if it doesn't exist.
			f := destCluster.Holder.Frame(src.Index, src.Frame)
			v := f.View(src.View)
			var err error
			destFragment, err = v.CreateFragmentIfNotExists(src.Slice)
			if err != nil {
				return err
			}
		}

		buf := bytes.NewBuffer(nil)

		bw := bufio.NewWriter(buf)
		br := bufio.NewReader(buf)

		// Get the fragment from source.
		if _, err := srcFragment.WriteTo(bw); err != nil {
			return err
		}

		// Flush the bufio.buf to the io.Writer (buf).
		bw.Flush()

		// Write data to destination.
		if _, err := destFragment.ReadFrom(br); err != nil {
			return err
		}
	}

	node := &pilosa.Node{
		URI: pilosa.DecodeURI(instr.Coordinator),
	}
	if err := t.SendTo(node, complete); err != nil {
		return err
	}

	return nil
}
