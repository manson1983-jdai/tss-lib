// Copyright © 2019 Binance
//
// This file is part of Binance. The full Binance copyright notice, including
// terms governing use, modification, and redistribution, is contained in the
// file LICENSE at the root of the source code distribution tree.

package resharing

import (
	"errors"
	"fmt"

	"github.com/manson1983-jdai/tss-lib/crypto"
	"github.com/manson1983-jdai/tss-lib/crypto/commitments"
	"github.com/manson1983-jdai/tss-lib/crypto/vss"
	"github.com/manson1983-jdai/tss-lib/eddsa/keygen"
	"github.com/manson1983-jdai/tss-lib/eddsa/signing"
	"github.com/manson1983-jdai/tss-lib/tss"
)

// round 1 represents round 1 of the keygen part of the EDDSA TSS spec
func newRound1(params *tss.ReSharingParameters, input, save *keygen.LocalPartySaveData, temp *localTempData, out chan<- tss.Message, end chan<- keygen.LocalPartySaveData) tss.Round {
	return &round1{
		&base{params, temp, input, save, out, end, make([]bool, len(params.OldParties().IDs())), make([]bool, len(params.NewParties().IDs())), false, 1}}
}

func (round *round1) Start() *tss.Error {
	if round.started {
		return round.WrapError(errors.New("round already started"))
	}
	round.number = 1
	round.started = true
	round.resetOK() // resets both round.oldOK and round.newOK
	round.allNewOK()

	if !round.ReSharingParams().IsOldCommittee() {
		return nil
	}
	round.allOldOK()

	Pi := round.PartyID()
	i := Pi.Index

	// 1. PrepareForSigning() -> w_i
	xi, ks := round.input.Xi, round.input.Ks
	if round.Threshold()+1 > len(ks) {
		return round.WrapError(fmt.Errorf("t+1=%d is not satisfied by the key count of %d", round.Threshold()+1, len(ks)), round.PartyID())
	}
	newKs := round.NewParties().IDs().Keys()
	wi := signing.PrepareForSigning(round.Params().EC(), i, len(round.OldParties().IDs()), xi, ks)

	// 2.
	vi, shares, err := vss.Create(round.Params().EC(), round.NewThreshold(), wi, newKs)
	if err != nil {
		return round.WrapError(err, round.PartyID())
	}

	// 3.
	flatVis, err := crypto.FlattenECPoints(vi)
	if err != nil {
		return round.WrapError(err, round.PartyID())
	}
	vCmt := commitments.NewHashCommitment(flatVis...)

	// 4. populate temp data
	round.temp.VD = vCmt.D
	round.temp.NewShares = shares

	// 5. "broadcast" C_i to members of the NEW committee
	r1msg := NewDGRound1Message(
		round.NewParties().IDs().Exclude(round.PartyID()), round.PartyID(),
		round.input.EDDSAPub, vCmt.C)
	round.temp.dgRound1Messages[i] = r1msg
	round.out <- r1msg

	return nil
}

func (round *round1) CanAccept(msg tss.ParsedMessage) bool {
	// accept messages from old -> new committee
	if _, ok := msg.Content().(*DGRound1Message); ok {
		return msg.IsBroadcast()
	}
	return false
}

func (round *round1) Update() (bool, *tss.Error) {
	// only the new committee receive in this round
	if !round.ReSharingParameters.IsNewCommittee() {
		return true, nil
	}
	// accept messages from old -> new committee
	for j, msg := range round.temp.dgRound1Messages {
		if round.oldOK[j] {
			continue
		}
		if msg == nil || !round.CanAccept(msg) {
			return false, nil
		}
		round.oldOK[j] = true

		// save the eddsa pub received from the old committee
		r1msg := round.temp.dgRound1Messages[0].Content().(*DGRound1Message)
		candidate, err := r1msg.UnmarshalEDDSAPub(round.Params().EC())
		if err != nil {
			return false, round.WrapError(errors.New("unable to unmarshal the eddsa pub key"), msg.GetFrom())
		}
		if round.save.EDDSAPub != nil &&
			!candidate.Equals(round.save.EDDSAPub) {
			// uh oh - anomaly!
			return false, round.WrapError(errors.New("eddsa pub key did not match what we received previously"), msg.GetFrom())
		}
		round.save.EDDSAPub = candidate
	}
	return true, nil
}

func (round *round1) NextRound() tss.Round {
	round.started = false
	return &round2{round}
}
