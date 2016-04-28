package skipchain

import (
	"crypto/rand"
	"errors"

	"fmt"

	"bytes"

	"strconv"

	"github.com/dedis/cothority/lib/cosi"
	"github.com/dedis/cothority/lib/dbg"
	"github.com/dedis/cothority/lib/network"
	"github.com/dedis/cothority/lib/sda"
)

// This file contains all the code to run a CoSi service. It is used to reply to
// client request for signing something using CoSi.
// As a prototype, it just signs and returns. It would be very easy to write an
// updated version that chains all signatures for example.

func init() {
	sda.RegisterNewService("Skipchain", newSkipchainService)
	skipchainSID = sda.ServiceFactory.ServiceID("Skipchain")
}

var skipchainSID sda.ServiceID

// Service handles adding new SkipBlocks
type Service struct {
	*sda.ServiceProcessor
	// SkipBlocks points from SkipBlockID to SkipBlock but SkipBlockID is not a valid
	// key-type for maps, so we need to cast it to string
	SkipBlocks map[string]SkipBlock
	path       string
}

// ProposeSkipBlock takes a hash for the latest valid SkipBlock and a SkipBlock
// that will be verified. If the verification returns true, the new SkipBlock
// will be signed and added to the chain and returned.
// If the given nil as the latest block it verify if we are actually creating
// the first (genesis) block and create it. If it is called with nil although
// there already exist previous blocks, it will return an error.
func (s *Service) ProposeSkipBlock(latest SkipBlockID, proposed SkipBlock) (*ProposedSkipBlockReply, error) {
	if latest == nil || len(latest) == 0 { // genesis block creation
		// TODO set real verifier
		proposed.GetCommon().VerifierId = VerifyNone
		s.updateNewSkipBlock(nil, proposed)
		reply := &ProposedSkipBlockReply{
			Previous: nil, // genesis block
			Latest:   proposed,
		}
		dbg.LLvl3(fmt.Sprintf("Successfuly created genesis: %+v", reply))
		_ = s.startPropagation(proposed)
		return reply, nil
	}

	prev, ok := s.SkipBlocks[string(latest)]
	if !ok {
		return nil, errors.New("Couldn't find latest block.")
	}
	proposed.GetCommon().VerifierId = prev.GetCommon().VerifierId
	if s.verifyNewSkipBlock(prev, proposed) {
		s.updateNewSkipBlock(prev, proposed)
		reply := &ProposedSkipBlockReply{
			Previous: prev,
			Latest:   proposed,
		}
		// notify all other services with the same ID:
		_ = s.startPropagation(proposed)
		return reply, nil
	}

	return nil, errors.New("Verification of proposed block failed.")
}
func (s *Service) ProposeSkipBlockData(e *network.Entity, psbd *ProposeSkipBlockData) (network.ProtocolMessage, error) {
	reply, err := s.ProposeSkipBlock(psbd.Latest, psbd.Proposed)
	if err != nil {
		return nil, err
	}
	return &ProposedSkipBlockReplyData{reply.Previous.(*SkipBlockData), reply.Latest.(*SkipBlockData)}, nil
}
func (s *Service) ProposeSkipBlockRoster(e *network.Entity, psbr *ProposeSkipBlockRoster) (network.ProtocolMessage, error) {
	reply, err := s.ProposeSkipBlock(psbr.Latest, psbr.Proposed)
	if err != nil {
		return nil, err
	}
	return &ProposedSkipBlockReplyRoster{reply.Previous.(*SkipBlockRoster), reply.Latest.(*SkipBlockRoster)}, nil
}

func (s *Service) updateNewSkipBlock(prev, proposed SkipBlock) {
	dbg.Lvl4(fmt.Sprintf("\nprev=%+v\nproposed=%+v", prev, proposed))
	sbc := proposed.GetCommon()
	// later we will support higher blocks
	sbc.Height = 1

	var curID string
	sbc.BackLink = make([]SkipBlockID, sbc.Height)
	if prev == nil { // genesis
		sbc.Index++
		// genesis block has a random back-link:
		bl := make([]byte, 32)
		_, _ = rand.Read(bl)
		sbc.BackLink[0] = bl
		// empty forward link:

		curID = string(proposed.updateHash())
	} else {
		prevCommon := prev.GetCommon()
		sbc.Index = prevCommon.Index + 1
		// TODO: add higher backlinks
		sbc.BackLink[0] = prev.updateHash()
		// update forward link of previous block:
		curHashID := proposed.updateHash()

		prevCommon.ForwardLink = make([]*BlockLink, 1) // TODO later with height
		prevCommon.ForwardLink[0] = NewBlockLink()
		prevCommon.ForwardLink[0].Hash = curHashID

		curID = string(curHashID)
	}
	// update
	s.SkipBlocks[curID] = proposed
}

// GetUpdateChain returns a slice of SkipBlocks which describe the part of the
// skipchain from the latest block the caller knows of to the actual latest
// SkipBlock.
// Somehow comparable to search in SkipLists.
func (s *Service) GetUpdateChain(e *network.Entity, u *GetUpdateChain) (network.ProtocolMessage, error) {
	block, ok := s.getSkipBlockByID(u.Latest)
	if !ok {
		return nil, errors.New("Couldn't find latest skipblock")
	}
	// at least the latest know and the next block:
	blocks := []SkipBlock{block}
	for len(block.GetCommon().ForwardLink) > 0 {
		// TODO: get highest forwardlink
		link := block.GetCommon().ForwardLink[0]
		block, ok = s.getSkipBlockByID(link.Hash)
		if !ok {
			return nil, errors.New("Missing block in forward-chain")
		}
		blocks = append(blocks, block)
	}
	reply := &GetUpdateChainReply{
		Update: blocks,
	}

	return reply, nil
}

// SetChildrenSkipBlock creates a new SkipChain if that 'service' doesn't exist
// yet.
func (s *Service) SetChildrenSkipBlock(parent, child SkipBlockID) error {
	parentBlock, ok := s.getSkipBlockByID(parent)
	if !ok {
		return errors.New("Couldn't find skipblock!")
	}
	childBlock, ok := s.getSkipBlockByID(child)
	if !ok {
		return errors.New("Couldn't find skipblock!")
	}
	pbRoster := parentBlock.(*SkipBlockRoster)
	cbc := childBlock.GetCommon()
	cbc.ParentBlock = parent
	pbRoster.ChildSL = NewBlockLink()
	pbRoster.ChildSL.Hash = child

	return nil
}

func (s *Service) getSkipBlockByID(sbID SkipBlockID) (SkipBlock, bool) {
	b, ok := s.SkipBlocks[string(sbID)]
	return b, ok
}

// GetChildrenSkipList creates a new SkipChain if that 'service' doesn't exist
// yet.
func (s *Service) GetChildrenSkipList(sb SkipBlock, verifier VerifierID) (*GetUpdateChainReply, error) {
	return nil, nil
}

// PropagateSkipBlock is called when a new SkipBlock or updated SkipBlock is
// available.
func (s *Service) PropagateSkipBlockData(e *network.Entity, latest *PropagateSkipBlockData) (network.ProtocolMessage, error) {
	dbg.Print("PropagateSkipBlock ....", s.Address())
	s.SkipBlocks[string(latest.SkipBlock.GetHash())] = latest.SkipBlock
	return nil, nil
}

// notify other services about new/updated skipblock
func (s *Service) startPropagation(latest SkipBlock) error {
	dbg.Print("Starting propagation of new block.")
	var el *sda.EntityList
	if dataBlock, isData := latest.(*SkipBlockData); isData {
		parent, ok := s.getSkipBlockByID(dataBlock.ParentBlock)
		if !ok {
			dbg.Error("Didn't find parent of data")
			return errors.New("Didn't find parent of data")
		}
		el = parent.(*SkipBlockRoster).EntityList

	} else {
		el = latest.(*SkipBlockRoster).EntityList
	}
	for i, e := range el.List {
		dbg.Print("Sending", strconv.Itoa(i))

		cr, err := sda.CreateClientRequest("Skipchain", &PropagateSkipBlock{latest})
		if err != nil {
			dbg.Error(err)
			return err
		}
		if err := s.SendRaw(e, cr); err != nil {
			dbg.Error(err)
			return err
		}
	}
	dbg.Print("Finished propagation")
	return nil
}

// SignBlock signs off the new block pointed to by the hash by first
// verifying its validity and then collectively signing off the block.
// The new signature is NOT broadcasted to the roster!
func (s *Service) SignBlock(sb SkipBlock) error {
	prev, ok := s.SkipBlocks[string(sb.GetCommon().BackLink[0])]
	if !ok {
		return errors.New("Didn't find SkipBlock")
	}
	if !s.verifyNewSkipBlock(prev, sb) {
		return errors.New("Refused")
	}
	// TODO: sign off the block with the roster
	sb.GetCommon().Signature = cosi.NewSignature(network.Suite)
	return nil
}

// ForwardSignature asks this responsible for a SkipChain to sign off
// a new ForwardLink. Upon success the new signature will be
// broadcast to the entire roster and all backward- and forward-links.
// It returns the SkipBlock with the updated ForwardSignature or an error.
func (s *Service) ForwardSignature(updating *ForwardSignature) (SkipBlock, error) {
	current, ok := s.SkipBlocks[string(updating.ToUpdate)]
	if !ok {
		return nil, errors.New("Didn't find SkipBlock")
	}
	if updating.Latest.VerifySignatures() != nil {
		return nil, errors.New("Couldn't verify signature of new block")
	}
	commCurrent := current.GetCommon()
	commLatest := updating.Latest.GetCommon()
	updateHeight := 0
	latestHeight := len(commLatest.BackLink)
	for updateHeight = 0; updateHeight < latestHeight; updateHeight++ {
		if bytes.Equal(commLatest.BackLink[updateHeight], commCurrent.Hash) {
			break
		}
	}
	if updateHeight == latestHeight {
		return nil, errors.New("Didn't find ourselves in the backlinks")
	}
	currHeight := len(commCurrent.ForwardLink)
	if currHeight == 0 {
		commCurrent.ForwardLink = make([]*BlockLink, 0, commCurrent.Height)
		// As we are the direct predecessor of the block, we need
		// to verify using the verification-function whether that
		// block is valid or not.
		if !s.verifyNewSkipBlock(current, updating.Latest) {
			return nil, errors.New("New SkipBlock not accepted!")
		}
	} else {
		// We only need to verify that we have a complete link-history
		// from ourselves to the proposed SkipBlock
		if !s.verifyLinkedSkipBlock(current, updating.Latest) {
			return nil, errors.New("Didn't find a valid update-path")
		}
	}
	commCurrent.ForwardLink[currHeight].Hash = updating.Latest.GetHash()

	// TODO: sign off on the forward-link (signature on hash of current and
	// following block)
	return current, nil
}

// NewProtocol is called on all nodes of a Tree (except the root, since it is
// the one starting the protocol) so it's the Service that will be called to
// generate the PI on all others node.
func (s *Service) NewProtocol(tn *sda.TreeNodeInstance, conf *sda.GenericConfig) (sda.ProtocolInstance, error) {
	dbg.Lvl1("SkipChain received New Protocol event", tn, conf)
	return nil, nil
}

// verifyNewSkipBlock calls the appropriate app-verification and returns
// either a signature on the newest SkipBlock or nil if the SkipBlock
// has been refused
func (s *Service) verifyNewSkipBlock(latest, newest SkipBlock) bool {
	// TODO: implement a couple of protocols that can check all
	// TODO: Verify* constants
	return newest.GetCommon().VerifierId == VerifyNone
}

// verifyLinkedSkipBlock checks if we have a valid link connecting the two
// SkipBlocks with each other.
func (s *Service) verifyLinkedSkipBlock(latest, newest SkipBlock) bool {
	// TODO: check we have a valid link
	return true
}

func newSkipchainService(c sda.Context, path string) sda.Service {
	s := &Service{
		ServiceProcessor: sda.NewServiceProcessor(c),
		path:             path,
		SkipBlocks:       make(map[string]SkipBlock),
	}
	if err := s.RegisterMessage(s.PropagateSkipBlockData); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	if err := s.RegisterMessage(s.ProposeSkipBlockData); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	if err := s.RegisterMessage(s.ProposeSkipBlockRoster); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	if err := s.RegisterMessage(s.GetUpdateChain); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	return s
}
