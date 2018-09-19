// Copyright 2018 The dexon-consensus-core Authors
// This file is part of the dexon-consensus-core library.
//
// The dexon-consensus-core library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus-core library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus-core library. If not, see
// <http://www.gnu.org/licenses/>.

package core

import (
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/dexon-foundation/dexon-consensus-core/core/test"
	"github.com/dexon-foundation/dexon-consensus-core/core/types"
	"github.com/dexon-foundation/dexon-consensus-core/crypto"
	"github.com/dexon-foundation/dexon-consensus-core/crypto/dkg"
	"github.com/dexon-foundation/dexon-consensus-core/crypto/eth"
)

type DKGTSIGProtocolTestSuite struct {
	suite.Suite

	vIDs    types.ValidatorIDs
	dkgIDs  map[types.ValidatorID]dkg.ID
	prvKeys map[types.ValidatorID]crypto.PrivateKey
}

type testDKGReceiver struct {
	s *DKGTSIGProtocolTestSuite

	prvKey     crypto.PrivateKey
	complaints map[types.ValidatorID]*types.DKGComplaint
	mpk        *types.DKGMasterPublicKey
	prvShare   map[types.ValidatorID]*types.DKGPrivateShare
}

func newTestDKGReceiver(
	s *DKGTSIGProtocolTestSuite, prvKey crypto.PrivateKey) *testDKGReceiver {
	return &testDKGReceiver{
		s:          s,
		prvKey:     prvKey,
		complaints: make(map[types.ValidatorID]*types.DKGComplaint),
		prvShare:   make(map[types.ValidatorID]*types.DKGPrivateShare),
	}
}

func (r *testDKGReceiver) ProposeDKGComplaint(complaint *types.DKGComplaint) {
	var err error
	complaint.Signature, err = r.prvKey.Sign(hashDKGComplaint(complaint))
	r.s.Require().NoError(err)
	r.complaints[complaint.ProposerID] = complaint
}

func (r *testDKGReceiver) ProposeDKGMasterPublicKey(
	mpk *types.DKGMasterPublicKey) {
	var err error
	mpk.Signature, err = r.prvKey.Sign(hashDKGMasterPublicKey(mpk))
	r.s.Require().NoError(err)
	r.mpk = mpk
}
func (r *testDKGReceiver) ProposeDKGPrivateShare(
	to types.ValidatorID, prv *types.DKGPrivateShare) {
	var err error
	prv.Signature, err = r.prvKey.Sign(hashDKGPrivateShare(prv))
	r.s.Require().NoError(err)
	r.prvShare[to] = prv
}

func (s *DKGTSIGProtocolTestSuite) setupDKGParticipants(n int) {
	s.vIDs = make(types.ValidatorIDs, 0, n)
	s.prvKeys = make(map[types.ValidatorID]crypto.PrivateKey, n)
	s.dkgIDs = make(map[types.ValidatorID]dkg.ID)
	ids := make(dkg.IDs, 0, n)
	for i := 0; i < n; i++ {
		prvKey, err := eth.NewPrivateKey()
		s.Require().NoError(err)
		vID := types.NewValidatorID(prvKey.PublicKey())
		s.vIDs = append(s.vIDs, vID)
		s.prvKeys[vID] = prvKey
		id := dkg.NewID(vID.Hash[:])
		ids = append(ids, id)
		s.dkgIDs[vID] = id
	}
}

// TestDKGTSIGProtocol will test the entire DKG+TISG protocol including
// exchanging private shares, recovering share secret, creating partial sign and
// recovering threshold signature.
// All participants are good people in this test.
func (s *DKGTSIGProtocolTestSuite) TestDKGTSIGProtocol() {
	k := 3
	n := 10
	round := uint64(1)
	gov, err := test.NewGovernance(5, 100)
	s.Require().NoError(err)
	s.setupDKGParticipants(n)

	receivers := make(map[types.ValidatorID]*testDKGReceiver, n)
	protocols := make(map[types.ValidatorID]*dkgProtocol, n)
	for _, vID := range s.vIDs {
		receivers[vID] = newTestDKGReceiver(s, s.prvKeys[vID])
		protocols[vID] = newDKGProtocol(
			vID,
			receivers[vID],
			round,
			k,
			eth.SigToPub,
		)
		s.Require().NotNil(receivers[vID].mpk)
	}

	for _, receiver := range receivers {
		gov.AddDKGMasterPublicKey(receiver.mpk)
	}

	for _, protocol := range protocols {
		s.Require().NoError(
			protocol.processMasterPublicKeys(gov.DKGMasterPublicKeys(round)))
	}

	for _, receiver := range receivers {
		s.Require().Len(receiver.prvShare, n)
		for vID, prvShare := range receiver.prvShare {
			s.Require().NoError(protocols[vID].processPrivateShare(prvShare))
		}
	}

	for _, recv := range receivers {
		s.Require().Len(recv.complaints, 0)
	}

	// DKG is fininished.
	gpk, err := newDKGGroupPublicKey(round,
		gov.DKGMasterPublicKeys(round), gov.DKGComplaints(round),
		k, eth.SigToPub,
	)
	s.Require().NoError(err)
	s.Require().Len(gpk.qualifyIDs, n)
	qualifyIDs := make(map[dkg.ID]struct{}, len(gpk.qualifyIDs))
	for _, id := range gpk.qualifyIDs {
		qualifyIDs[id] = struct{}{}
	}

	shareSecrets := make(
		map[types.ValidatorID]*dkgShareSecret, len(qualifyIDs))

	for vID, protocol := range protocols {
		_, exist := qualifyIDs[s.dkgIDs[vID]]
		s.Require().True(exist)
		var err error
		shareSecrets[vID], err = protocol.recoverShareSecret(gpk.qualifyIDs)
		s.Require().NoError(err)
	}

	tsig := newTSigProtocol(gpk)
	msgHash := crypto.Keccak256Hash([]byte("🏖🍹"))
	for vID, shareSecret := range shareSecrets {
		psig := &types.DKGPartialSignature{
			ProposerID:       vID,
			Round:            round,
			PartialSignature: shareSecret.sign(msgHash),
		}
		var err error
		psig.Signature, err = s.prvKeys[vID].Sign(hashDKGPartialSignature(psig))
		s.Require().NoError(err)
		s.Require().NoError(tsig.processPartialSignature(msgHash, psig))
		if len(tsig.sigs) > k {
			break
		}
	}

	sig, err := tsig.signature()
	s.Require().NoError(err)
	s.True(gpk.verifySignature(msgHash, sig))
}

func TestDKGTSIGProtocol(t *testing.T) {
	suite.Run(t, new(DKGTSIGProtocolTestSuite))
}