// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package avalanche

import (
	"github.com/ava-labs/gecko/ids"
	"github.com/ava-labs/gecko/snow"
	"github.com/ava-labs/gecko/snow/choices"
	"github.com/ava-labs/gecko/snow/consensus/snowstorm"
)

// TopologicalFactory implements Factory by returning a topological struct
type TopologicalFactory struct{}

// New implements Factory
func (TopologicalFactory) New() Consensus { return &Topological{} }

// TODO: Implement pruning of decisions.
// To perfectly preserve the protocol, this implementation will need to store
// the hashes of all accepted decisions. It is possible to add a heuristic that
// removes sufficiently old decisions. However, that will need to be analyzed to
// ensure safety. It is doable when adding in a weak synchrony assumption.

// Topological performs the avalanche algorithm by utilizing a topological sort
// of the voting results. Assumes that vertices are inserted in topological
// order.
type Topological struct {
	metrics

	// Context used for logging
	ctx *snow.Context
	// Threshold for confidence increases
	params Parameters

	// Maps vtxID -> vtx
	nodes map[[32]byte]Vertex
	// Tracks the conflict relations
	cg snowstorm.Consensus

	// preferred is the frontier of vtxIDs that are strongly preferred
	// virtuous is the frontier of vtxIDs that are strongly virtuous
	// orphans are the txIDs that are virtuous, but not preferred
	preferred, virtuous, orphans ids.Set
	// frontier is the set of vts that have no descendents
	frontier map[[32]byte]Vertex
	// preferenceCache is the cache for strongly preferred checks
	// virtuousCache is the cache for strongly virtuous checks
	preferenceCache, virtuousCache map[[32]byte]bool
}

type kahnNode struct {
	inDegree int
	votes    ids.BitSet
}

// Initialize implements the Avalanche interface
func (ta *Topological) Initialize(ctx *snow.Context, params Parameters, frontier []Vertex) {
	ctx.Log.AssertDeferredNoError(params.Valid)

	ta.ctx = ctx
	ta.params = params

	if err := ta.metrics.Initialize(ctx.Log, params.Namespace, params.Metrics); err != nil {
		ta.ctx.Log.Error("%s", err)
	}

	ta.nodes = make(map[[32]byte]Vertex)

	ta.cg = &snowstorm.Directed{}
	ta.cg.Initialize(ctx, params.Parameters)

	ta.frontier = make(map[[32]byte]Vertex)
	for _, vtx := range frontier {
		ta.frontier[vtx.ID().Key()] = vtx
	}
	ta.updateFrontiers()
}

// Parameters implements the Avalanche interface
func (ta *Topological) Parameters() Parameters { return ta.params }

// IsVirtuous implements the Avalanche interface
func (ta *Topological) IsVirtuous(tx snowstorm.Tx) bool { return ta.cg.IsVirtuous(tx) }

// Add implements the Avalanche interface
func (ta *Topological) Add(vtx Vertex) {
	ta.ctx.Log.AssertTrue(vtx != nil, "Attempting to insert nil vertex")

	vtxID := vtx.ID()
	key := vtxID.Key()
	if vtx.Status().Decided() {
		return // Already decided this vertex
	} else if _, exists := ta.nodes[key]; exists {
		return // Already inserted this vertex
	}

	ta.ctx.ConsensusDispatcher.Issue(ta.ctx.ChainID, vtxID, vtx.Bytes())

	for _, tx := range vtx.Txs() {
		if !tx.Status().Decided() {
			// Add the consumers to the conflict graph.
			ta.cg.Add(tx)
		}
	}

	ta.nodes[key] = vtx // Add this vertex to the set of nodes
	ta.metrics.Issued(vtxID)

	ta.update(vtx) // Update the vertex and it's ancestry
}

// VertexIssued implements the Avalanche interface
func (ta *Topological) VertexIssued(vtx Vertex) bool {
	if vtx.Status().Decided() {
		return true
	}
	_, ok := ta.nodes[vtx.ID().Key()]
	return ok
}

// TxIssued implements the Avalanche interface
func (ta *Topological) TxIssued(tx snowstorm.Tx) bool { return ta.cg.Issued(tx) }

// Orphans implements the Avalanche interface
func (ta *Topological) Orphans() ids.Set { return ta.orphans }

// Virtuous implements the Avalanche interface
func (ta *Topological) Virtuous() ids.Set { return ta.virtuous }

// Preferences implements the Avalanche interface
func (ta *Topological) Preferences() ids.Set { return ta.preferred }

// RecordPoll implements the Avalanche interface
func (ta *Topological) RecordPoll(responses ids.UniqueBag) {
	// Set up the topological sort: O(|Live Set|)
	kahns, leaves := ta.calculateInDegree(responses)
	// Collect the votes for each transaction: O(|Live Set|)
	votes := ta.pushVotes(kahns, leaves)
	// Update the conflict graph: O(|Transactions|)
	ta.ctx.Log.Verbo("Updating consumer confidences based on:\n%s", &votes)
	ta.cg.RecordPoll(votes)
	// Update the dag: O(|Live Set|)
	ta.updateFrontiers()
}

// Quiesce implements the Avalanche interface
func (ta *Topological) Quiesce() bool { return ta.cg.Quiesce() }

// Finalized implements the Avalanche interface
func (ta *Topological) Finalized() bool { return ta.cg.Finalized() }

// Takes in a list of votes and sets up the topological ordering. Returns the
// reachable section of the graph annotated with the number of inbound edges and
// the non-transitively applied votes. Also returns the list of leaf nodes.
func (ta *Topological) calculateInDegree(
	responses ids.UniqueBag) (map[[32]byte]kahnNode, []ids.ID) {
	kahns := make(map[[32]byte]kahnNode)
	leaves := ids.Set{}

	for _, vote := range responses.List() {
		key := vote.Key()
		// If it is not found, then the vote is either for something decided,
		// or something we haven't heard of yet.
		if vtx := ta.nodes[key]; vtx != nil {
			kahn, previouslySeen := kahns[key]
			// Add this new vote to the current bag of votes
			kahn.votes.Union(responses.GetSet(vote))
			kahns[key] = kahn

			if !previouslySeen {
				// If I've never seen this node before, it is currently a leaf.
				leaves.Add(vote)
				ta.markAncestorInDegrees(kahns, leaves, vtx.Parents())
			}
		}
	}

	return kahns, leaves.List()
}

// adds a new in-degree reference for all nodes
func (ta *Topological) markAncestorInDegrees(
	kahns map[[32]byte]kahnNode,
	leaves ids.Set,
	deps []Vertex) (map[[32]byte]kahnNode, ids.Set) {
	frontier := []Vertex{}
	for _, vtx := range deps {
		// The vertex may have been decided, no need to vote in that case
		if !vtx.Status().Decided() {
			frontier = append(frontier, vtx)
		}
	}

	for len(frontier) > 0 {
		newLen := len(frontier) - 1
		current := frontier[newLen]
		frontier = frontier[:newLen]

		currentID := current.ID()
		currentKey := currentID.Key()
		kahn, alreadySeen := kahns[currentKey]
		// I got here through a transitive edge, so increase the in-degree
		kahn.inDegree++
		kahns[currentKey] = kahn

		if kahn.inDegree == 1 {
			// If I am transitively seeing this node for the first
			// time, it is no longer a leaf.
			leaves.Remove(currentID)
		}

		if !alreadySeen {
			// If I am seeing this node for the first time, I need to check its
			// parents
			for _, depVtx := range current.Parents() {
				// No need to traverse to a decided vertex
				if !depVtx.Status().Decided() {
					frontier = append(frontier, depVtx)
				}
			}
		}
	}
	return kahns, leaves
}

// count the number of votes for each operation
func (ta *Topological) pushVotes(
	kahnNodes map[[32]byte]kahnNode,
	leaves []ids.ID) ids.Bag {
	votes := make(ids.UniqueBag)

	for len(leaves) > 0 {
		newLeavesSize := len(leaves) - 1
		leaf := leaves[newLeavesSize]
		leaves = leaves[:newLeavesSize]

		key := leaf.Key()
		kahn := kahnNodes[key]

		if vtx := ta.nodes[key]; vtx != nil {
			for _, tx := range vtx.Txs() {
				// Give the votes to the consumer
				txID := tx.ID()
				votes.UnionSet(txID, kahn.votes)
			}

			for _, dep := range vtx.Parents() {
				depID := dep.ID()
				depKey := depID.Key()
				if depNode, notPruned := kahnNodes[depKey]; notPruned {
					depNode.inDegree--
					// Give the votes to my parents
					depNode.votes.Union(kahn.votes)
					kahnNodes[depKey] = depNode

					if depNode.inDegree == 0 {
						// Only traverse into the leaves
						leaves = append(leaves, depID)
					}
				}
			}
		}
	}

	return votes.Bag(ta.params.Alpha)
}

// If I've already checked, do nothing
// If I'm decided, cache the preference and return
// At this point, I must be live
// I now try to accept all my consumers
// I now update all my ancestors
// If any of my parents are rejected, reject myself
// If I'm preferred, remove all my ancestors from the preferred frontier, add
//     myself to the preferred frontier
// If all my parents are accepted and I'm acceptable, accept myself
func (ta *Topological) update(vtx Vertex) {
	vtxID := vtx.ID()
	vtxKey := vtxID.Key()
	if _, cached := ta.preferenceCache[vtxKey]; cached {
		return // This vertex has already been updated
	}

	switch vtx.Status() {
	case choices.Accepted:
		ta.preferred.Add(vtxID) // I'm preferred
		ta.virtuous.Add(vtxID)  // Accepted is defined as virtuous

		ta.frontier[vtxKey] = vtx // I have no descendents yet

		ta.preferenceCache[vtxKey] = true
		ta.virtuousCache[vtxKey] = true
		return
	case choices.Rejected:
		// I'm rejected
		ta.preferenceCache[vtxKey] = false
		ta.virtuousCache[vtxKey] = false
		return
	}

	acceptable := true  // If the batch is accepted, this vertex is acceptable
	rejectable := false // If I'm rejectable, I must be rejected
	preferred := true
	virtuous := true
	txs := vtx.Txs()
	preferences := ta.cg.Preferences()
	virtuousTxs := ta.cg.Virtuous()

	for _, tx := range txs {
		txID := tx.ID()
		s := tx.Status()
		if s == choices.Rejected {
			// If I contain a rejected consumer, I am rejectable
			rejectable = true
			preferred = false
			virtuous = false
		}
		if s != choices.Accepted {
			// If I contain a non-accepted consumer, I am not acceptable
			acceptable = false
			preferred = preferred && preferences.Contains(txID)
			virtuous = virtuous && virtuousTxs.Contains(txID)
		}
	}

	deps := vtx.Parents()
	// Update all of my dependencies
	for _, dep := range deps {
		ta.update(dep)

		depID := dep.ID()
		key := depID.Key()
		preferred = preferred && ta.preferenceCache[key]
		virtuous = virtuous && ta.virtuousCache[key]
	}

	// Check my parent statuses
	for _, dep := range deps {
		if status := dep.Status(); status == choices.Rejected {
			vtx.Reject() // My parent is rejected, so I should be rejected
			delete(ta.nodes, vtxKey)
			ta.metrics.Rejected(vtxID)

			ta.preferenceCache[vtxKey] = false
			ta.virtuousCache[vtxKey] = false
			return
		} else if status != choices.Accepted {
			acceptable = false // My parent isn't accepted, so I can't be
		}
	}

	// Technically, we could also check to see if there are direct conflicts
	// between this vertex and a vertex in it's ancestry. If there does exist
	// such a conflict, this vertex could also be rejected. However, this would
	// require a traversal. Therefore, this memory optimization is ignored.
	// Also, this will only happen from a byzantine node issuing the vertex.
	// Therefore, this is very unlikely to actually be triggered in practice.

	// Remove all my parents from the frontier
	for _, dep := range deps {
		delete(ta.frontier, dep.ID().Key())
	}
	ta.frontier[vtxKey] = vtx // I have no descendents yet

	ta.preferenceCache[vtxKey] = preferred
	ta.virtuousCache[vtxKey] = virtuous

	if preferred {
		ta.preferred.Add(vtxID) // I'm preferred
		for _, dep := range deps {
			ta.preferred.Remove(dep.ID()) // My parents aren't part of the frontier
		}

		for _, tx := range txs {
			if tx.Status() != choices.Accepted {
				ta.orphans.Remove(tx.ID())
			}
		}
	}

	if virtuous {
		ta.virtuous.Add(vtxID) // I'm virtuous
		for _, dep := range deps {
			ta.virtuous.Remove(dep.ID()) // My parents aren't part of the frontier
		}
	}

	switch {
	case acceptable:
		// I'm acceptable, why not accept?
		ta.ctx.ConsensusDispatcher.Accept(ta.ctx.ChainID, vtxID, vtx.Bytes())
		vtx.Accept()
		delete(ta.nodes, vtxKey)
		ta.metrics.Accepted(vtxID)
	case rejectable:
		// I'm rejectable, why not reject?
		vtx.Reject()
		ta.ctx.ConsensusDispatcher.Reject(ta.ctx.ChainID, vtxID, vtx.Bytes())
		delete(ta.nodes, vtxKey)
		ta.metrics.Rejected(vtxID)
	}
}

// Update the frontier sets
func (ta *Topological) updateFrontiers() {
	vts := ta.frontier

	ta.preferred.Clear()
	ta.virtuous.Clear()
	ta.orphans.Clear()
	ta.frontier = make(map[[32]byte]Vertex)
	ta.preferenceCache = make(map[[32]byte]bool)
	ta.virtuousCache = make(map[[32]byte]bool)

	ta.orphans.Union(ta.cg.Virtuous()) // Initially, nothing is preferred

	for _, vtx := range vts {
		// Update all the vertices that were in my previous frontier
		ta.update(vtx)
	}
}
