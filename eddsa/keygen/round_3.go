// Copyright © 2019 Binance
//
// This file is part of Binance. The full Binance copyright notice, including
// terms governing use, modification, and redistribution, is contained in the
// file LICENSE at the root of the source code distribution tree.

package keygen

import (
	"errors"
	"math/big"

	"github.com/hashicorp/go-multierror"
	errors2 "github.com/pkg/errors"

	"github.com/manson1983-jdai/tss-lib/common"
	"github.com/manson1983-jdai/tss-lib/crypto"
	"github.com/manson1983-jdai/tss-lib/crypto/commitments"
	"github.com/manson1983-jdai/tss-lib/crypto/vss"
	"github.com/manson1983-jdai/tss-lib/tss"
)

func (round *round3) Start() *tss.Error {
	if round.started {
		return round.WrapError(errors.New("round already started"))
	}
	round.number = 3
	round.started = true
	round.resetOK()

	Ps := round.Parties().IDs()
	PIdx := round.PartyID().Index

	// 1,10. calculate xi
	xi := new(big.Int).Set(round.temp.shares[PIdx].Share)
	for j := range Ps {
		if j == PIdx {
			continue
		}
		r2msg1 := round.temp.kgRound2Message1s[j].Content().(*KGRound2Message1)
		share := r2msg1.UnmarshalShare()
		xi = new(big.Int).Add(xi, share)
	}
	round.save.Xi = new(big.Int).Mod(xi, round.Params().EC().Params().N)

	// 2-3.
	Vc := make(vss.Vs, round.Threshold()+1)
	for c := range Vc {
		Vc[c] = round.temp.vs[c] // ours
	}

	// 4-12.
	type vssOut struct {
		unWrappedErr error
		pjVs         vss.Vs
	}
	chs := make([]chan vssOut, len(Ps))
	for i := range chs {
		if i == PIdx {
			continue
		}
		chs[i] = make(chan vssOut)
	}
	for j := range Ps {
		if j == PIdx {
			continue
		}
		// 6-9.
		go func(j int, ch chan<- vssOut) {
			// 4-10.
			KGCj := round.temp.KGCs[j]
			r2msg2 := round.temp.kgRound2Message2s[j].Content().(*KGRound2Message2)
			KGDj := r2msg2.UnmarshalDeCommitment()
			cmtDeCmt := commitments.HashCommitDecommit{C: KGCj, D: KGDj}
			ok, flatPolyGs := cmtDeCmt.DeCommit()
			if !ok || flatPolyGs == nil {
				ch <- vssOut{errors.New("de-commitment verify failed"), nil}
				return
			}

			PjVs, err := crypto.UnFlattenECPoints(round.Params().EC(), flatPolyGs)
			for i, PjV := range PjVs {
				PjVs[i] = PjV.EightInvEight()
			}

			if err != nil {
				ch <- vssOut{err, nil}
				return
			}
			proof, err := r2msg2.UnmarshalZKProof(round.Params().EC())
			if err != nil {
				ch <- vssOut{errors.New("failed to unmarshal schnorr proof"), nil}
				return
			}
			ok = proof.Verify(PjVs[0])
			if !ok {
				ch <- vssOut{errors.New("failed to prove schnorr proof"), nil}
				return
			}
			r2msg1 := round.temp.kgRound2Message1s[j].Content().(*KGRound2Message1)
			PjShare := vss.Share{
				Threshold: round.Threshold(),
				ID:        round.PartyID().KeyInt(),
				Share:     r2msg1.UnmarshalShare(),
			}
			if ok = PjShare.Verify(round.Params().EC(), round.Threshold(), PjVs); !ok {
				ch <- vssOut{errors.New("vss verify failed"), nil}
				return
			}
			// (9) handled above
			ch <- vssOut{nil, PjVs}
		}(j, chs[j])
	}

	// consume unbuffered channels (end the goroutines)
	vssResults := make([]vssOut, len(Ps))
	{
		culprits := make([]*tss.PartyID, 0, len(Ps)) // who caused the error(s)
		for j, Pj := range Ps {
			if j == PIdx {
				continue
			}
			vssResults[j] = <-chs[j]
			// collect culprits to error out with
			if err := vssResults[j].unWrappedErr; err != nil {
				culprits = append(culprits, Pj)
			}
		}
		var multiErr error
		if len(culprits) > 0 {
			for _, vssResult := range vssResults {
				if vssResult.unWrappedErr == nil {
					continue
				}
				multiErr = multierror.Append(multiErr, vssResult.unWrappedErr)
			}
			return round.WrapError(multiErr, culprits...)
		}
	}
	{
		var err error
		culprits := make([]*tss.PartyID, 0, len(Ps)) // who caused the error(s)
		for j, Pj := range Ps {
			if j == PIdx {
				continue
			}
			// 11-12.
			PjVs := vssResults[j].pjVs
			for c := 0; c <= round.Threshold(); c++ {
				Vc[c], err = Vc[c].Add(PjVs[c])
				if err != nil {
					culprits = append(culprits, Pj)
				}
			}
		}
		if len(culprits) > 0 {
			return round.WrapError(errors.New("adding PjVs[c] to Vc[c] resulted in a point not on the curve"), culprits...)
		}
	}

	// 13-17. compute Xj for each Pj
	{
		var err error
		modQ := common.ModInt(round.Params().EC().Params().N)
		culprits := make([]*tss.PartyID, 0, len(Ps)) // who caused the error(s)
		bigXj := round.save.BigXj
		for j := 0; j < round.PartyCount(); j++ {
			Pj := round.Parties().IDs()[j]
			kj := Pj.KeyInt()
			BigXj := Vc[0]
			z := new(big.Int).SetInt64(int64(1))
			for c := 1; c <= round.Threshold(); c++ {
				z = modQ.Mul(z, kj)
				BigXj, err = BigXj.Add(Vc[c].ScalarMult(z))
				if err != nil {
					culprits = append(culprits, Pj)
				}
			}
			bigXj[j] = BigXj
		}
		if len(culprits) > 0 {
			return round.WrapError(errors.New("adding Vc[c].ScalarMult(z) to BigXj resulted in a point not on the curve"), culprits...)
		}
		round.save.BigXj = bigXj
	}

	// 18. compute and SAVE the EDDSA public key `y`
	eddsaPubKey, err := crypto.NewECPoint(round.Params().EC(), Vc[0].X(), Vc[0].Y())
	if err != nil {
		return round.WrapError(errors2.Wrapf(err, "public key is not on the curve"))
	}
	round.save.EDDSAPub = eddsaPubKey

	// PRINT public key & private share
	common.Logger.Debugf("%s public key: %x", round.PartyID(), eddsaPubKey)

	round.end <- *round.save
	return nil
}

func (round *round3) CanAccept(msg tss.ParsedMessage) bool {
	// not expecting any incoming messages in this round
	return false
}

func (round *round3) Update() (bool, *tss.Error) {
	// not expecting any incoming messages in this round
	return false, nil
}

func (round *round3) NextRound() tss.Round {
	return nil // finished!
}
