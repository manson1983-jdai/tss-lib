// Copyright © 2019 Binance
//
// This file is part of Binance. The full Binance copyright notice, including
// terms governing use, modification, and redistribution, is contained in the
// file LICENSE at the root of the source code distribution tree.

package resharing_test

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/ipfs/go-log"
	"github.com/stretchr/testify/assert"

	"github.com/manson1983-jdai/tss-lib/common"
	"github.com/manson1983-jdai/tss-lib/crypto"
	"github.com/manson1983-jdai/tss-lib/ecdsa/keygen"
	. "github.com/manson1983-jdai/tss-lib/ecdsa/resharing"
	"github.com/manson1983-jdai/tss-lib/ecdsa/signing"
	"github.com/manson1983-jdai/tss-lib/test"
	"github.com/manson1983-jdai/tss-lib/tss"
)

const (
	testParticipants = test.TestParticipants
	testThreshold    = test.TestThreshold
)

func setUp(level string) {
	if err := log.SetLogLevel("tss-lib", level); err != nil {
		panic(err)
	}
}

func TestE2EConcurrent(t *testing.T) {
	setUp("info")

	// tss.SetCurve(elliptic.P256())

	threshold, newThreshold := testThreshold, testThreshold

	// PHASE: load keygen fixtures
	firstPartyIdx, extraParties := 5, 1 // extra can be 0 to N-first
	oldKeys, oldPIDs, err := keygen.LoadKeygenTestFixtures(testThreshold+1+extraParties+firstPartyIdx, firstPartyIdx)
	assert.NoError(t, err, "should load keygen fixtures")

	// PHASE: resharing
	oldP2PCtx := tss.NewPeerContext(oldPIDs)
	// init the new parties; re-use the fixture pre-params for speed
	fixtures, _, err := keygen.LoadKeygenTestFixtures(testParticipants)
	if err != nil {
		common.Logger.Info("No test fixtures were found, so the safe primes will be generated from scratch. This may take a while...")
	}
	newPIDs := tss.GenerateTestPartyIDs(testParticipants)
	newP2PCtx := tss.NewPeerContext(newPIDs)
	newPCount := len(newPIDs)

	oldCommittee := make([]*LocalParty, 0, len(oldPIDs))
	newCommittee := make([]*LocalParty, 0, newPCount)
	bothCommitteesPax := len(oldCommittee) + len(newCommittee)

	errCh := make(chan *tss.Error, bothCommitteesPax)
	outCh := make(chan tss.Message, bothCommitteesPax)
	endCh := make(chan keygen.LocalPartySaveData, bothCommitteesPax)

	updater := test.SharedPartyUpdater

	// init the old parties first
	for j, pID := range oldPIDs {
		params := tss.NewReSharingParameters(tss.S256(), oldP2PCtx, newP2PCtx, pID, testParticipants, threshold, newPCount, newThreshold)
		P := NewLocalParty(params, oldKeys[j], outCh, endCh).(*LocalParty) // discard old key data
		oldCommittee = append(oldCommittee, P)
	}
	// init the new parties
	for j, pID := range newPIDs {
		params := tss.NewReSharingParameters(tss.S256(), oldP2PCtx, newP2PCtx, pID, testParticipants, threshold, newPCount, newThreshold)
		save := keygen.NewLocalPartySaveData(newPCount)
		if j < len(fixtures) && len(newPIDs) <= len(fixtures) {
			save.LocalPreParams = fixtures[j].LocalPreParams
		}
		P := NewLocalParty(params, save, outCh, endCh).(*LocalParty)
		newCommittee = append(newCommittee, P)
	}

	// start the new parties; they will wait for messages
	for _, P := range newCommittee {
		go func(P *LocalParty) {
			if err := P.Start(); err != nil {
				errCh <- err
			}
		}(P)
	}
	// start the old parties; they will send messages
	for _, P := range oldCommittee {
		go func(P *LocalParty) {
			if err := P.Start(); err != nil {
				errCh <- err
			}
		}(P)
	}

	newKeys := make([]keygen.LocalPartySaveData, len(newCommittee))
	endedOldCommittee := 0
	var reSharingEnded int32
	for {
		fmt.Printf("ACTIVE GOROUTINES: %d\n", runtime.NumGoroutine())
		select {
		case err := <-errCh:
			common.Logger.Errorf("Error: %s", err)
			assert.FailNow(t, err.Error())
			return

		case msg := <-outCh:
			dest := msg.GetTo()
			if dest == nil {
				t.Fatal("did not expect a msg to have a nil destination during resharing")
			}
			if msg.IsToOldCommittee() || msg.IsToOldAndNewCommittees() {
				for _, destP := range dest[:len(oldCommittee)] {
					go updater(oldCommittee[destP.Index], msg, errCh)
				}
			}
			if !msg.IsToOldCommittee() || msg.IsToOldAndNewCommittees() {
				for _, destP := range dest {
					go updater(newCommittee[destP.Index], msg, errCh)
				}
			}

		case save := <-endCh:
			// old committee members that aren't receiving a share have their Xi zeroed
			if save.Xi != nil {
				index, err := save.OriginalIndex()
				assert.NoErrorf(t, err, "should not be an error getting a party's index from save data")
				newKeys[index] = save
			} else {
				endedOldCommittee++
			}
			atomic.AddInt32(&reSharingEnded, 1)
			if atomic.LoadInt32(&reSharingEnded) == int32(len(oldCommittee)+len(newCommittee)) {
				assert.Equal(t, len(oldCommittee), endedOldCommittee)
				t.Logf("Resharing done. Reshared %d participants", reSharingEnded)

				// xj tests: BigXj == xj*G
				for j, key := range newKeys {
					// xj test: BigXj == xj*G
					xj := key.Xi
					gXj := crypto.ScalarBaseMult(tss.S256(), xj)
					BigXj := key.BigXj[j]
					assert.True(t, BigXj.Equals(gXj), "ensure BigX_j == g^x_j")
				}

				// more verification of signing is implemented within local_party_test.go of keygen package
				goto signing
			}
		}
	}

signing:
	// PHASE: signing
	signKeys, signPIDs := newKeys, newPIDs
	signP2pCtx := tss.NewPeerContext(signPIDs)
	signParties := make([]*signing.LocalParty, 0, len(signPIDs))

	signErrCh := make(chan *tss.Error, len(signPIDs))
	signOutCh := make(chan tss.Message, len(signPIDs))
	signEndCh := make(chan common.SignatureData, len(signPIDs))

	for j, signPID := range signPIDs {
		params := tss.NewParameters(tss.S256(), signP2pCtx, signPID, len(signPIDs), newThreshold)
		P := signing.NewLocalParty(big.NewInt(42), params, signKeys[j], signOutCh, signEndCh).(*signing.LocalParty)
		signParties = append(signParties, P)
		go func(P *signing.LocalParty) {
			if err := P.Start(); err != nil {
				signErrCh <- err
			}
		}(P)
	}

	var signEnded int32
	for {
		fmt.Printf("ACTIVE GOROUTINES: %d\n", runtime.NumGoroutine())
		select {
		case err := <-signErrCh:
			common.Logger.Errorf("Error: %s", err)
			assert.FailNow(t, err.Error())
			return

		case msg := <-signOutCh:
			dest := msg.GetTo()
			if dest == nil {
				for _, P := range signParties {
					if P.PartyID().Index == msg.GetFrom().Index {
						continue
					}
					go updater(P, msg, signErrCh)
				}
			} else {
				if dest[0].Index == msg.GetFrom().Index {
					t.Fatalf("party %d tried to send a message to itself (%d)", dest[0].Index, msg.GetFrom().Index)
				}
				go updater(signParties[dest[0].Index], msg, signErrCh)
			}

		case signData := <-signEndCh:
			atomic.AddInt32(&signEnded, 1)
			if atomic.LoadInt32(&signEnded) == int32(len(signPIDs)) {
				t.Logf("Signing done. Received sign data from %d participants", signEnded)

				// BEGIN ECDSA verify
				pkX, pkY := signKeys[0].ECDSAPub.X(), signKeys[0].ECDSAPub.Y()
				pk := ecdsa.PublicKey{
					Curve: tss.S256(),
					X:     pkX,
					Y:     pkY,
				}
				ok := ecdsa.Verify(&pk, big.NewInt(42).Bytes(),
					new(big.Int).SetBytes(signData.R),
					new(big.Int).SetBytes(signData.S))

				assert.True(t, ok, "ecdsa verify must pass")
				t.Log("ECDSA signing test done.")
				// END ECDSA verify

				return
			}
		}
	}
}
