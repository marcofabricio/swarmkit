package raft_test

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/manager/state/raft"
	"github.com/docker/swarmkit/manager/state/raft/storage"
	raftutils "github.com/docker/swarmkit/manager/state/raft/testutils"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/pivotal-golang/clock/fakeclock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

func TestRaftSnapshot(t *testing.T) {
	t.Parallel()

	// Bring up a 3 node cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc, &api.RaftConfig{SnapshotInterval: 9, LogEntriesForSlowFollowers: 0})
	defer raftutils.TeardownCluster(t, nodes)

	nodeIDs := []string{"id1", "id2", "id3", "id4", "id5", "id6", "id7", "id8", "id9", "id10", "id11", "id12"}
	values := make([]*api.Node, len(nodeIDs))
	snapshotFilenames := make(map[uint64]string, 4)

	// Propose 3 values
	var err error
	for i, nodeID := range nodeIDs[:3] {
		values[i], err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, nodeID)
		assert.NoError(t, err, "failed to propose value")
	}

	// None of the nodes should have snapshot files yet
	for _, node := range nodes {
		dirents, err := ioutil.ReadDir(filepath.Join(node.StateDir, "snap-v3-encrypted"))
		assert.NoError(t, err)
		assert.Len(t, dirents, 0)
	}

	// Check all nodes have all the data.
	// This also acts as a synchronization point so that the next value we
	// propose will arrive as a separate message to the raft state machine,
	// and it is guaranteed to have the right cluster settings when
	// deciding whether to create a new snapshot.
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs[:3], values)

	// Propose a 4th value
	values[3], err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, nodeIDs[3])
	assert.NoError(t, err, "failed to propose value")

	// All nodes should now have a snapshot file
	for nodeID, node := range nodes {
		assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
			dirents, err := ioutil.ReadDir(filepath.Join(node.StateDir, "snap-v3-encrypted"))
			if err != nil {
				return err
			}
			if len(dirents) != 1 {
				return fmt.Errorf("expected 1 snapshot, found %d", len(dirents))
			}
			snapshotFilenames[nodeID] = dirents[0].Name()
			return nil
		}))
	}

	// Add a node to the cluster
	raftutils.AddRaftNode(t, clockSource, nodes, tc)

	// It should get a copy of the snapshot
	assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
		dirents, err := ioutil.ReadDir(filepath.Join(nodes[4].StateDir, "snap-v3-encrypted"))
		if err != nil {
			return err
		}
		if len(dirents) != 1 {
			return fmt.Errorf("expected 1 snapshot, found %d on new node", len(dirents))
		}
		snapshotFilenames[4] = dirents[0].Name()
		return nil
	}))

	// It should know about the other nodes
	stripMembers := func(memberList map[uint64]*api.RaftMember) map[uint64]*api.RaftMember {
		raftNodes := make(map[uint64]*api.RaftMember)
		for k, v := range memberList {
			raftNodes[k] = &api.RaftMember{
				RaftID: v.RaftID,
				Addr:   v.Addr,
			}
		}
		return raftNodes
	}
	assert.Equal(t, stripMembers(nodes[1].GetMemberlist()), stripMembers(nodes[4].GetMemberlist()))

	// All nodes should have all the data
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs[:4], values)

	// Propose more values to provoke a second snapshot
	for i := 4; i != len(nodeIDs); i++ {
		values[i], err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, nodeIDs[i])
		assert.NoError(t, err, "failed to propose value")
	}

	// All nodes should have a snapshot under a *different* name
	for nodeID, node := range nodes {
		assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
			dirents, err := ioutil.ReadDir(filepath.Join(node.StateDir, "snap-v3-encrypted"))
			if err != nil {
				return err
			}
			if len(dirents) != 1 {
				return fmt.Errorf("expected 1 snapshot, found %d on node %d", len(dirents), nodeID)
			}
			if dirents[0].Name() == snapshotFilenames[nodeID] {
				return fmt.Errorf("snapshot %s did not get replaced on node %d", snapshotFilenames[nodeID], nodeID)
			}
			return nil
		}))
	}

	// All nodes should have all the data
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)
}

func TestRaftSnapshotRestart(t *testing.T) {
	t.Parallel()

	// Bring up a 3 node cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc, &api.RaftConfig{SnapshotInterval: 10, LogEntriesForSlowFollowers: 0})
	defer raftutils.TeardownCluster(t, nodes)

	nodeIDs := []string{"id1", "id2", "id3", "id4", "id5", "id6", "id7"}
	values := make([]*api.Node, len(nodeIDs))

	// Propose 3 values
	var err error
	for i, nodeID := range nodeIDs[:3] {
		values[i], err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, nodeID)
		assert.NoError(t, err, "failed to propose value")
	}

	// Take down node 3
	nodes[3].Server.Stop()
	nodes[3].ShutdownRaft()

	// Propose a 4th value before the snapshot
	values[3], err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, nodeIDs[3])
	assert.NoError(t, err, "failed to propose value")

	// Remaining nodes shouldn't have snapshot files yet
	for _, node := range []*raftutils.TestNode{nodes[1], nodes[2]} {
		dirents, err := ioutil.ReadDir(filepath.Join(node.StateDir, "snap-v3-encrypted"))
		assert.NoError(t, err)
		assert.Len(t, dirents, 0)
	}

	// Add a node to the cluster before the snapshot. This is the event
	// that triggers the snapshot.
	nodes[4] = raftutils.NewJoinNode(t, clockSource, nodes[1].Address, tc)
	raftutils.WaitForCluster(t, clockSource, map[uint64]*raftutils.TestNode{1: nodes[1], 2: nodes[2], 4: nodes[4]})

	// Remaining nodes should now have a snapshot file
	for nodeIdx, node := range []*raftutils.TestNode{nodes[1], nodes[2]} {
		assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
			dirents, err := ioutil.ReadDir(filepath.Join(node.StateDir, "snap-v3-encrypted"))
			if err != nil {
				return err
			}
			if len(dirents) != 1 {
				return fmt.Errorf("expected 1 snapshot, found %d on node %d", len(dirents), nodeIdx+1)
			}
			return nil
		}))
	}
	raftutils.CheckValuesOnNodes(t, clockSource, map[uint64]*raftutils.TestNode{1: nodes[1], 2: nodes[2]}, nodeIDs[:4], values[:4])

	// Propose a 5th value
	values[4], err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, nodeIDs[4])
	require.NoError(t, err)

	// Add another node to the cluster
	nodes[5] = raftutils.NewJoinNode(t, clockSource, nodes[1].Address, tc)
	raftutils.WaitForCluster(t, clockSource, map[uint64]*raftutils.TestNode{1: nodes[1], 2: nodes[2], 4: nodes[4], 5: nodes[5]})

	// New node should get a copy of the snapshot
	assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
		dirents, err := ioutil.ReadDir(filepath.Join(nodes[5].StateDir, "snap-v3-encrypted"))
		if err != nil {
			return err
		}
		if len(dirents) != 1 {
			return fmt.Errorf("expected 1 snapshot, found %d on new node", len(dirents))
		}
		return nil
	}))

	dirents, err := ioutil.ReadDir(filepath.Join(nodes[5].StateDir, "snap-v3-encrypted"))
	assert.NoError(t, err)
	assert.Len(t, dirents, 1)
	raftutils.CheckValuesOnNodes(t, clockSource, map[uint64]*raftutils.TestNode{1: nodes[1], 2: nodes[2]}, nodeIDs[:5], values[:5])

	// It should know about the other nodes, including the one that was just added
	stripMembers := func(memberList map[uint64]*api.RaftMember) map[uint64]*api.RaftMember {
		raftNodes := make(map[uint64]*api.RaftMember)
		for k, v := range memberList {
			raftNodes[k] = &api.RaftMember{
				RaftID: v.RaftID,
				Addr:   v.Addr,
			}
		}
		return raftNodes
	}
	assert.Equal(t, stripMembers(nodes[1].GetMemberlist()), stripMembers(nodes[4].GetMemberlist()))

	// Restart node 3
	nodes[3] = raftutils.RestartNode(t, clockSource, nodes[3], false)
	raftutils.WaitForCluster(t, clockSource, nodes)

	// Node 3 should know about other nodes, including the new one
	assert.Len(t, nodes[3].GetMemberlist(), 5)
	assert.Equal(t, stripMembers(nodes[1].GetMemberlist()), stripMembers(nodes[3].GetMemberlist()))

	// Propose yet another value, to make sure the rejoined node is still
	// receiving new logs
	values[5], err = raftutils.ProposeValue(t, raftutils.Leader(nodes), DefaultProposalTime, nodeIDs[5])
	require.NoError(t, err)

	// All nodes should have all the data
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs[:6], values[:6])

	// Restart node 3 again. It should load the snapshot.
	nodes[3].Server.Stop()
	nodes[3].ShutdownRaft()
	nodes[3] = raftutils.RestartNode(t, clockSource, nodes[3], false)
	raftutils.WaitForCluster(t, clockSource, nodes)

	assert.Len(t, nodes[3].GetMemberlist(), 5)
	assert.Equal(t, stripMembers(nodes[1].GetMemberlist()), stripMembers(nodes[3].GetMemberlist()))
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs[:6], values[:6])

	// Propose again. Just to check consensus after this latest restart.
	values[6], err = raftutils.ProposeValue(t, raftutils.Leader(nodes), DefaultProposalTime, nodeIDs[6])
	require.NoError(t, err)
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)
}

func TestGCWAL(t *testing.T) {
	t.Parallel()

	// Additional log entries from cluster setup, leader election
	extraLogEntries := 5
	// Number of large entries to propose
	proposals := 8

	// Bring up a 3 node cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc, &api.RaftConfig{SnapshotInterval: uint64(proposals + extraLogEntries), LogEntriesForSlowFollowers: 0})

	for i := 0; i != proposals; i++ {
		_, err := proposeLargeValue(t, nodes[1], DefaultProposalTime, fmt.Sprintf("id%d", i))
		assert.NoError(t, err, "failed to propose value")
	}

	time.Sleep(250 * time.Millisecond)

	// Snapshot should have been triggered just as the WAL rotated, so
	// both WAL files should be preserved
	assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
		dirents, err := ioutil.ReadDir(filepath.Join(nodes[1].StateDir, "snap-v3-encrypted"))
		if err != nil {
			return err
		}
		if len(dirents) != 1 {
			return fmt.Errorf("expected 1 snapshot, found %d", len(dirents))
		}

		dirents, err = ioutil.ReadDir(filepath.Join(nodes[1].StateDir, "wal-v3-encrypted"))
		if err != nil {
			return err
		}
		var walCount int
		for _, f := range dirents {
			if strings.HasSuffix(f.Name(), ".wal") {
				walCount++
			}
		}
		if walCount != 2 {
			return fmt.Errorf("expected 2 WAL files, found %d", walCount)
		}
		return nil
	}))

	raftutils.TeardownCluster(t, nodes)

	// Repeat this test, but trigger the snapshot after the WAL has rotated
	proposals++
	nodes, clockSource = raftutils.NewRaftCluster(t, tc, &api.RaftConfig{SnapshotInterval: uint64(proposals + extraLogEntries), LogEntriesForSlowFollowers: 0})
	defer raftutils.TeardownCluster(t, nodes)

	for i := 0; i != proposals; i++ {
		_, err := proposeLargeValue(t, nodes[1], DefaultProposalTime, fmt.Sprintf("id%d", i))
		assert.NoError(t, err, "failed to propose value")
	}

	time.Sleep(250 * time.Millisecond)

	// This time only one WAL file should be saved.
	assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
		dirents, err := ioutil.ReadDir(filepath.Join(nodes[1].StateDir, "snap-v3-encrypted"))
		if err != nil {
			return err
		}

		if len(dirents) != 1 {
			return fmt.Errorf("expected 1 snapshot, found %d", len(dirents))
		}

		dirents, err = ioutil.ReadDir(filepath.Join(nodes[1].StateDir, "wal-v3-encrypted"))
		if err != nil {
			return err
		}
		var walCount int
		for _, f := range dirents {
			if strings.HasSuffix(f.Name(), ".wal") {
				walCount++
			}
		}
		if walCount != 1 {
			return fmt.Errorf("expected 1 WAL file, found %d", walCount)
		}
		return nil
	}))

	// Restart the whole cluster
	for _, node := range nodes {
		node.Server.Stop()
		node.ShutdownRaft()
	}

	raftutils.AdvanceTicks(clockSource, 5)

	i := 0
	for k, node := range nodes {
		nodes[k] = raftutils.RestartNode(t, clockSource, node, false)
		i++
	}
	raftutils.WaitForCluster(t, clockSource, nodes)

	// Is the data intact after restart?
	for _, node := range nodes {
		assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
			var err error
			node.MemoryStore().View(func(tx store.ReadTx) {
				var allNodes []*api.Node
				allNodes, err = store.FindNodes(tx, store.All)
				if err != nil {
					return
				}
				if len(allNodes) != proposals {
					err = fmt.Errorf("expected %d nodes, got %d", proposals, len(allNodes))
					return
				}
			})
			return err
		}))
	}

	// It should still be possible to propose values
	_, err := raftutils.ProposeValue(t, raftutils.Leader(nodes), DefaultProposalTime, "newnode")
	assert.NoError(t, err, "failed to propose value")

	for _, node := range nodes {
		assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
			var err error
			node.MemoryStore().View(func(tx store.ReadTx) {
				var allNodes []*api.Node
				allNodes, err = store.FindNodes(tx, store.All)
				if err != nil {
					return
				}
				if len(allNodes) != proposals+1 {
					err = fmt.Errorf("expected %d nodes, got %d", proposals, len(allNodes))
					return
				}
			})
			return err
		}))
	}
}

// proposeLargeValue proposes a 10kb value to a raft test cluster
func proposeLargeValue(t *testing.T, raftNode *raftutils.TestNode, time time.Duration, nodeID ...string) (*api.Node, error) {
	nodeIDStr := "id1"
	if len(nodeID) != 0 {
		nodeIDStr = nodeID[0]
	}
	a := make([]byte, 10000)
	for i := 0; i != len(a); i++ {
		a[i] = 'a'
	}
	node := &api.Node{
		ID: nodeIDStr,
		Spec: api.NodeSpec{
			Annotations: api.Annotations{
				Name: nodeIDStr,
				Labels: map[string]string{
					"largestring": string(a),
				},
			},
		},
	}

	storeActions := []*api.StoreAction{
		{
			Action: api.StoreActionKindCreate,
			Target: &api.StoreAction_Node{
				Node: node,
			},
		},
	}

	ctx, _ := context.WithTimeout(context.Background(), time)

	err := raftNode.ProposeValue(ctx, storeActions, func() {
		err := raftNode.MemoryStore().ApplyStoreActions(storeActions)
		assert.NoError(t, err, "error applying actions")
	})
	if err != nil {
		return nil, err
	}

	return node, nil
}

// This test rotates the encryption key and waits for the expected thing to happen
func TestRaftEncryptionKeyRotationWait(t *testing.T) {
	t.Parallel()
	nodes := make(map[uint64]*raftutils.TestNode)
	var clockSource *fakeclock.FakeClock

	raftConfig := raft.DefaultRaftConfig()
	nodes[1], clockSource = raftutils.NewInitNode(t, tc, &raftConfig)
	defer raftutils.TeardownCluster(t, nodes)

	nodeIDs := []string{"id1", "id2", "id3"}
	values := make([]*api.Node, len(nodeIDs))

	// Propose 3 values
	var err error
	for i, nodeID := range nodeIDs[:3] {
		values[i], err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, nodeID)
		require.NoError(t, err, "failed to propose value")
	}

	snapDir := filepath.Join(nodes[1].StateDir, "snap-v3-encrypted")

	startingKeys := nodes[1].KeyRotator.GetKeys()

	// rotate the encryption key
	nodes[1].KeyRotator.QueuePendingKey([]byte("key2"))
	nodes[1].KeyRotator.RotationNotify() <- struct{}{}

	// the rotation should trigger a snapshot, which should notify the rotator when it's done
	require.NoError(t, raftutils.PollFunc(clockSource, func() error {
		snapshots, err := storage.ListSnapshots(snapDir)
		if err != nil {
			return err
		}
		if len(snapshots) != 1 {
			return fmt.Errorf("expected 1 snapshot, found %d on new node", len(snapshots))
		}
		if nodes[1].KeyRotator.NeedsRotation() {
			return fmt.Errorf("rotation never finished")
		}
		return nil
	}))
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)

	// Propose a 4th value
	nodeIDs = append(nodeIDs, "id4")
	v, err := raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, "id4")
	require.NoError(t, err, "failed to propose value")
	values = append(values, v)
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)

	nodes[1].Server.Stop()
	nodes[1].ShutdownRaft()

	// Try to restart node 1. Without the new unlock key, it can't actually start
	n, ctx := raftutils.CopyNode(t, clockSource, nodes[1], false, raftutils.NewSimpleKeyRotator(startingKeys))
	require.Error(t, n.Node.JoinAndStart(ctx),
		"should not have been able to restart since we can't read snapshots")

	// with the right key, it can start, even if the right key is only the pending key
	newKeys := startingKeys
	newKeys.PendingDEK = []byte("key2")
	nodes[1].KeyRotator = raftutils.NewSimpleKeyRotator(newKeys)
	nodes[1] = raftutils.RestartNode(t, clockSource, nodes[1], false)

	raftutils.WaitForCluster(t, clockSource, nodes)

	// as soon as we joined, it should have finished rotating the key
	require.False(t, nodes[1].KeyRotator.NeedsRotation())
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)

	// break snapshotting, and ensure that key rotation never finishes
	tempSnapDir := filepath.Join(nodes[1].StateDir, "snap-backup")
	require.NoError(t, os.Rename(snapDir, tempSnapDir))
	require.NoError(t, ioutil.WriteFile(snapDir, []byte("this is no longer a directory"), 0644))

	nodes[1].KeyRotator.QueuePendingKey([]byte("key3"))
	nodes[1].KeyRotator.RotationNotify() <- struct{}{}

	time.Sleep(250 * time.Millisecond)

	// rotation has not been finished, because we cannot take a snapshot
	require.True(t, nodes[1].KeyRotator.NeedsRotation())

	// Propose a 5th value, so we have WALs written with the new key
	nodeIDs = append(nodeIDs, "id5")
	v, err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, "id5")
	require.NoError(t, err, "failed to propose value")
	values = append(values, v)
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)

	nodes[1].Server.Stop()
	nodes[1].ShutdownRaft()

	// restore the snapshot dir
	require.NoError(t, os.RemoveAll(snapDir))
	require.NoError(t, os.Rename(tempSnapDir, snapDir))

	// Now the wals are a mix of key2 and key3 - we can't actually start with either key
	singleKey := raft.EncryptionKeys{CurrentDEK: []byte("key2")}
	n, ctx = raftutils.CopyNode(t, clockSource, nodes[1], false, raftutils.NewSimpleKeyRotator(singleKey))
	require.Error(t, n.Node.JoinAndStart(ctx),
		"should not have been able to restart since we can't read all the WALs, even if we can read the snapshot")
	singleKey = raft.EncryptionKeys{CurrentDEK: []byte("key3")}
	n, ctx = raftutils.CopyNode(t, clockSource, nodes[1], false, raftutils.NewSimpleKeyRotator(singleKey))
	require.Error(t, n.Node.JoinAndStart(ctx),
		"should not have been able to restart since we can't read all the WALs, and also not the snapshot")

	nodes[1], ctx = raftutils.CopyNode(t, clockSource, nodes[1], false,
		raftutils.NewSimpleKeyRotator(raft.EncryptionKeys{
			CurrentDEK: []byte("key2"),
			PendingDEK: []byte("key3"),
		}))
	require.NoError(t, nodes[1].Node.JoinAndStart(ctx))

	// we can load, but we still need a snapshot because rotation hasn't finished
	snapshots, err := storage.ListSnapshots(snapDir)
	require.NoError(t, err)
	require.Len(t, snapshots, 1, "expected 1 snapshot")
	require.True(t, nodes[1].KeyRotator.NeedsRotation())
	currSnapshot := snapshots[0]

	// start the node - everything should fix itself
	go nodes[1].Node.Run(ctx)
	raftutils.WaitForCluster(t, clockSource, nodes)

	require.NoError(t, raftutils.PollFunc(clockSource, func() error {
		snapshots, err := storage.ListSnapshots(snapDir)
		if err != nil {
			return err
		}
		if len(snapshots) != 1 {
			return fmt.Errorf("expected 1 snapshots, found %d on new node", len(snapshots))
		}
		if snapshots[0] == currSnapshot {
			return fmt.Errorf("new snapshot not done yet")
		}
		if nodes[1].KeyRotator.NeedsRotation() {
			return fmt.Errorf("rotation never finished")
		}
		currSnapshot = snapshots[0]
		return nil
	}))
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)

	// If we can't update the keys, we wait for the next snapshot to do so
	nodes[1].KeyRotator.SetUpdateFunc(func() error { return fmt.Errorf("nope!") })
	nodes[1].KeyRotator.QueuePendingKey([]byte("key4"))
	nodes[1].KeyRotator.RotationNotify() <- struct{}{}

	require.NoError(t, raftutils.PollFunc(clockSource, func() error {
		snapshots, err := storage.ListSnapshots(snapDir)
		if err != nil {
			return err
		}
		if len(snapshots) != 1 {
			return fmt.Errorf("expected 1 snapshots, found %d on new node", len(snapshots))
		}
		if snapshots[0] == currSnapshot {
			return fmt.Errorf("new snapshot not done yet")
		}
		currSnapshot = snapshots[0]
		return nil
	}))
	require.True(t, nodes[1].KeyRotator.NeedsRotation())

	// Fix updating the key rotator, and propose a 6th value - this should trigger the key
	// rotation to finish
	nodes[1].KeyRotator.SetUpdateFunc(nil)
	nodeIDs = append(nodeIDs, "id6")
	v, err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, "id6")
	require.NoError(t, err, "failed to propose value")
	values = append(values, v)
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)

	require.NoError(t, raftutils.PollFunc(clockSource, func() error {
		if nodes[1].KeyRotator.NeedsRotation() {
			return fmt.Errorf("rotation never finished")
		}
		return nil
	}))

	// no new snapshot
	snapshots, err = storage.ListSnapshots(snapDir)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	require.Equal(t, currSnapshot, snapshots[0])

	// Even if something goes wrong with getting keys, and needs rotation returns a false positive,
	// if there's no PendingDEK nothing happens.

	fakeTrue := true
	nodes[1].KeyRotator.SetNeedsRotation(&fakeTrue)
	nodes[1].KeyRotator.RotationNotify() <- struct{}{}

	// propose another value
	nodeIDs = append(nodeIDs, "id7")
	v, err = raftutils.ProposeValue(t, nodes[1], DefaultProposalTime, "id7")
	require.NoError(t, err, "failed to propose value")
	values = append(values, v)
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)

	// no new snapshot
	snapshots, err = storage.ListSnapshots(snapDir)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	require.Equal(t, currSnapshot, snapshots[0])

	// and when we restart, we can restart with the original key (the WAL written for the new proposed value)
	// is written with the old key
	nodes[1].Server.Stop()
	nodes[1].ShutdownRaft()

	nodes[1].KeyRotator = raftutils.NewSimpleKeyRotator(raft.EncryptionKeys{
		CurrentDEK: []byte("key4"),
	})
	nodes[1] = raftutils.RestartNode(t, clockSource, nodes[1], false)
	raftutils.WaitForCluster(t, clockSource, nodes)
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, nodeIDs, values)
}

// This test rotates the encryption key and restarts the node - the intent is try to trigger
// race conditions if there is more than one node and hence consensus may take longer.
func TestRaftEncryptionKeyRotationStress(t *testing.T) {
	t.Parallel()

	// Bring up a 3 nodes cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)
	leader := nodes[1]
	third := nodes[3]

	// constantly propose values
	done, stop := make(chan struct{}), make(chan struct{})
	go func() {
		counter := len(nodes)
		for {
			select {
			case <-stop:
				close(done)
				return
			default:
				counter += 1
				raftutils.ProposeValue(t, leader, DefaultProposalTime, fmt.Sprintf("id%d", counter))
			}
		}
	}()

	for i := 0; i < 30; i++ {
		// rotate the encryption key
		third.KeyRotator.QueuePendingKey([]byte(fmt.Sprintf("newKey%d", i)))
		third.KeyRotator.RotationNotify() <- struct{}{}

		require.NoError(t, raftutils.PollFunc(clockSource, func() error {
			if third.KeyRotator.GetKeys().PendingDEK == nil {
				third.Server.Stop()
				third.ShutdownRaft()
				third = raftutils.RestartNode(t, clockSource, third, false)
				raftutils.AdvanceTicks(clockSource, 1)
				return nil
			}
			return fmt.Errorf("not done rotating yet")
		}))
	}

	close(stop)
	<-done
}
