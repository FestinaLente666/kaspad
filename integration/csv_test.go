// Copyright (c) 2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// This file is ignored during the regular tests due to the following build tag.
// +build rpctest

package integration

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/daglabs/btcd/blockdag"
	"github.com/daglabs/btcd/btcec"
	"github.com/daglabs/btcd/dagconfig"
	"github.com/daglabs/btcd/dagconfig/daghash"
	"github.com/daglabs/btcd/integration/rpctest"
	"github.com/daglabs/btcd/txscript"
	"github.com/daglabs/btcd/wire"
	"github.com/daglabs/btcd/util"
)

// makeTestOutput creates an on-chain output paying to a freshly generated
// p2pkh output with the specified amount.
func makeTestOutput(r *rpctest.Harness, t *testing.T,
	amt util.Amount) (*btcec.PrivateKey, *wire.OutPoint, []byte, error) {

	// Create a fresh key, then send some coins to an address spendable by
	// that key.
	key, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, nil, err
	}

	// Using the key created above, generate a pkScript which it's able to
	// spend.
	a, err := util.NewAddressPubKey(key.PubKey().SerializeCompressed(), r.ActiveNet)
	if err != nil {
		return nil, nil, nil, err
	}
	selfAddrScript, err := txscript.PayToAddrScript(a.AddressPubKeyHash())
	if err != nil {
		return nil, nil, nil, err
	}
	output := &wire.TxOut{PkScript: selfAddrScript, Value: 1e8}

	// Next, create and broadcast a transaction paying to the output.
	fundTx, err := r.CreateTransaction([]*wire.TxOut{output}, 10)
	if err != nil {
		return nil, nil, nil, err
	}
	txHash, err := r.Node.SendRawTransaction(fundTx, true)
	if err != nil {
		return nil, nil, nil, err
	}

	// The transaction created above should be included within the next
	// generated block.
	blockHash, err := r.Node.Generate(1)
	if err != nil {
		return nil, nil, nil, err
	}
	assertTxInBlock(r, t, blockHash[0], txHash)

	// Locate the output index of the coins spendable by the key we
	// generated above, this is needed in order to create a proper utxo for
	// this output.
	var outputIndex uint32
	if bytes.Equal(fundTx.TxOut[0].PkScript, selfAddrScript) {
		outputIndex = 0
	} else {
		outputIndex = 1
	}

	utxo := &wire.OutPoint{
		Hash:  fundTx.TxHash(),
		Index: outputIndex,
	}

	return key, utxo, selfAddrScript, nil
}

// TestBIP0113Activation tests for proper adherence of the BIP 113 rule
// constraint which requires all transaction finality tests to use the MTP of
// the last 11 blocks, rather than the timestamp of the block which includes
// them.
//
// Overview:
//    - Transactions with non-final lock-times from the PoV of MTP should be
//      rejected from the mempool and when found within otherwise valid blocks.
//    - Transactions with final lock-times from the PoV of MTP should be
//      accepted to the mempool and mined in future block.
func TestBIP0113(t *testing.T) {
	t.Parallel()

	btcdCfg := []string{"--rejectnonstd"}
	r, err := rpctest.New(&dagconfig.SimNetParams, nil, btcdCfg)
	if err != nil {
		t.Fatal("unable to create primary harness: ", err)
	}
	if err := r.SetUp(true, 1); err != nil {
		t.Fatalf("unable to setup test chain: %v", err)
	}
	defer r.TearDown()

	// Create a fresh output for usage within the test below.
	const outputValue = util.SatoshiPerBitcoin
	outputKey, testOutput, testPkScript, err := makeTestOutput(r, t,
		outputValue)
	if err != nil {
		t.Fatalf("unable to create test output: %v", err)
	}

	// Fetch a fresh address from the harness, we'll use this address to
	// send funds back into the Harness.
	addr, err := r.NewAddress()
	if err != nil {
		t.Fatalf("unable to generate address: %v", err)
	}
	addrScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("unable to generate addr script: %v", err)
	}

	// Now create a transaction with a lock time which is "final" according
	// to the latest block, but not according to the current median time
	// past.
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *testOutput,
	})
	tx.AddTxOut(&wire.TxOut{
		PkScript: addrScript,
		Value:    outputValue - 1000,
	})

	// We set the lock-time of the transaction to just one minute after the
	// current MTP of the chain.
	chainInfo, err := r.Node.GetBlockChainInfo()
	if err != nil {
		t.Fatalf("unable to query for chain info: %v", err)
	}
	tx.LockTime = chainInfo.MedianTime + 1

	sigScript, err := txscript.SignatureScript(tx, 0, testPkScript,
		txscript.SigHashAll, outputKey, true)
	if err != nil {
		t.Fatalf("unable to generate sig: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript

	// This transaction should be rejected from the mempool as using MTP
	// for transactions finality is now a policy rule. Additionally, the
	// exact error should be the rejection of a non-final transaction.
	_, err = r.Node.SendRawTransaction(tx, true)
	if err == nil {
		t.Fatalf("transaction accepted, but should be non-final")
	} else if !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("transaction should be rejected due to being "+
			"non-final, instead: %v", err)
	}

	// The timeLockDeltas slice represents a series of deviations from the
	// current MTP which will be used to test border conditions w.r.t
	// transaction finality. -1 indicates 1 second prior to the MTP, 0
	// indicates the current MTP, and 1 indicates 1 second after the
	// current MTP.
	//
	// This time, all transactions which are final according to the MTP
	// *should* be accepted to both the mempool and within a valid block.
	// While transactions with lock-times *after* the current MTP should be
	// rejected.
	timeLockDeltas := []int64{-1, 0, 1}
	for _, timeLockDelta := range timeLockDeltas {
		chainInfo, err = r.Node.GetBlockChainInfo()
		if err != nil {
			t.Fatalf("unable to query for chain info: %v", err)
		}
		medianTimePast := chainInfo.MedianTime

		// Create another test output to be spent shortly below.
		outputKey, testOutput, testPkScript, err = makeTestOutput(r, t,
			outputValue)
		if err != nil {
			t.Fatalf("unable to create test output: %v", err)
		}

		// Create a new transaction with a lock-time past the current known
		// MTP.
		tx = wire.NewMsgTx(1)
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *testOutput,
		})
		tx.AddTxOut(&wire.TxOut{
			PkScript: addrScript,
			Value:    outputValue - 1000,
		})
		tx.LockTime = medianTimePast + timeLockDelta
		sigScript, err = txscript.SignatureScript(tx, 0, testPkScript,
			txscript.SigHashAll, outputKey, true)
		if err != nil {
			t.Fatalf("unable to generate sig: %v", err)
		}
		tx.TxIn[0].SignatureScript = sigScript

		// If the time-lock delta is greater than -1, then the
		// transaction should be rejected from the mempool and when
		// included within a block. A time-lock delta of -1 should be
		// accepted as it has a lock-time of one
		// second _before_ the current MTP.

		_, err = r.Node.SendRawTransaction(tx, true)
		if err == nil && timeLockDelta >= 0 {
			t.Fatal("transaction was accepted into the mempool " +
				"but should be rejected!")
		} else if err != nil && !strings.Contains(err.Error(), "not finalized") {
			t.Fatalf("transaction should be rejected from mempool "+
				"due to being  non-final, instead: %v", err)
		}

		txns := []*util.Tx{util.NewTx(tx)}
		_, err := r.GenerateAndSubmitBlock(txns, -1, time.Time{})
		if err == nil && timeLockDelta >= 0 {
			t.Fatal("block should be rejected due to non-final " +
				"txn, but was accepted")
		} else if err != nil && !strings.Contains(err.Error(), "unfinalized") {
			t.Fatalf("block should be rejected due to non-final "+
				"tx, instead: %v", err)
		}
	}
}

// createCSVOutput creates an output paying to a trivially redeemable CSV
// pkScript with the specified time-lock.
func createCSVOutput(r *rpctest.Harness, t *testing.T,
	numSatoshis util.Amount, timeLock int64,
	isSeconds bool) ([]byte, *wire.OutPoint, *wire.MsgTx, error) {

	// Convert the time-lock to the proper sequence lock based according to
	// if the lock is seconds or time based.
	sequenceLock := blockdag.LockTimeToSequence(isSeconds,
		int64(timeLock))

	// Our CSV script is simply: <sequenceLock> OP_CSV
	b := txscript.NewScriptBuilder().
		AddInt64(int64(sequenceLock)).
		AddOp(txscript.OpCheckSequenceVerify)
	csvScript, err := b.Script()
	if err != nil {
		return nil, nil, nil, err
	}

	// Using the script generated above, create a P2SH output which will be
	// accepted into the mempool.
	p2shAddr, err := util.NewAddressScriptHash(csvScript, r.ActiveNet)
	if err != nil {
		return nil, nil, nil, err
	}
	p2shScript, err := txscript.PayToAddrScript(p2shAddr)
	if err != nil {
		return nil, nil, nil, err
	}
	output := &wire.TxOut{
		PkScript: p2shScript,
		Value:    int64(numSatoshis),
	}

	// Finally create a valid transaction which creates the output crafted
	// above.
	tx, err := r.CreateTransaction([]*wire.TxOut{output}, 10)
	if err != nil {
		return nil, nil, nil, err
	}

	var outputIndex uint32
	if !bytes.Equal(tx.TxOut[0].PkScript, p2shScript) {
		outputIndex = 1
	}

	utxo := &wire.OutPoint{
		Hash:  tx.TxHash(),
		Index: outputIndex,
	}

	return csvScript, utxo, tx, nil
}

// spendCSVOutput spends an output previously created by the createCSVOutput
// function. The sigScript is a trivial push of OP_TRUE followed by the
// redeemScript to pass P2SH evaluation.
func spendCSVOutput(redeemScript []byte, csvUTXO *wire.OutPoint,
	sequence uint64, targetOutput *wire.TxOut,
	txVersion int32) (*wire.MsgTx, error) {

	tx := wire.NewMsgTx(txVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *csvUTXO,
		Sequence:         sequence,
	})
	tx.AddTxOut(targetOutput)

	b := txscript.NewScriptBuilder().
		AddOp(txscript.OpTrue).
		AddData(redeemScript)

	sigScript, err := b.Script()
	if err != nil {
		return nil, err
	}
	tx.TxIn[0].SignatureScript = sigScript

	return tx, nil
}

// assertTxInBlock asserts a transaction with the specified txid is found
// within the block with the passed block hash.
func assertTxInBlock(r *rpctest.Harness, t *testing.T, blockHash *daghash.Hash,
	txid *daghash.Hash) {

	block, err := r.Node.GetBlock(blockHash)
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}
	if len(block.Transactions) < 2 {
		t.Fatal("target transaction was not mined")
	}

	for _, txn := range block.Transactions {
		txHash := txn.TxHash()
		if txn.TxHash() == txHash {
			return
		}
	}

	_, _, line, _ := runtime.Caller(1)
	t.Fatalf("assertion failed at line %v: txid %v was not found in "+
		"block %v", line, txid, blockHash)
}

// TestBIP0068AndCsv tests for the proper adherence to the BIP 68
// rule-set and the behaviour of OP_CHECKSEQUENCEVERIFY
func TestBIP0068AndCsv(t *testing.T) {
	t.Parallel()

	// We'd like the test proper evaluation and validation of the BIP 68
	// (sequence locks) and BIP 112 rule-sets which add input-age based
	// relative lock times.

	btcdCfg := []string{"--rejectnonstd"}
	r, err := rpctest.New(&dagconfig.SimNetParams, nil, btcdCfg)
	if err != nil {
		t.Fatal("unable to create primary harness: ", err)
	}
	if err := r.SetUp(true, 1); err != nil {
		t.Fatalf("unable to setup test chain: %v", err)
	}
	defer r.TearDown()

	harnessAddr, err := r.NewAddress()
	if err != nil {
		t.Fatalf("unable to obtain harness address: %v", err)
	}
	harnessScript, err := txscript.PayToAddrScript(harnessAddr)
	if err != nil {
		t.Fatalf("unable to generate pkScript: %v", err)
	}

	const (
		outputAmt         = util.SatoshiPerBitcoin
		relativeBlockLock = 10
	)

	sweepOutput := &wire.TxOut{
		Value:    outputAmt - 5000,
		PkScript: harnessScript,
	}

	// With the height at 104 we need 200 blocks to be mined after the
	// genesis target period, so we mine 192 blocks. This'll put us at
	// height 296. The getblockchaininfo call checks the state for the
	// block AFTER the current height.
	numBlocks := (r.ActiveNet.MinerConfirmationWindow * 2) - 8
	if _, err := r.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	assertChainHeight(r, t, 293)

	// Knowing the number of outputs needed for the tests below, create a
	// fresh output for use within each of the test-cases below.
	const relativeTimeLock = 512
	const numTests = 7
	type csvOutput struct {
		RedeemScript []byte
		Utxo         *wire.OutPoint
		Timelock     int64
	}
	var spendableInputs [numTests]csvOutput

	// Create three outputs which have a block-based sequence locks, and
	// three outputs which use the above time based sequence lock.
	for i := 0; i < numTests; i++ {
		timeLock := relativeTimeLock
		isSeconds := true
		if i < 6 {
			timeLock = relativeBlockLock
			isSeconds = false
		}

		redeemScript, utxo, tx, err := createCSVOutput(r, t, outputAmt,
			timeLock, isSeconds)
		if err != nil {
			t.Fatalf("unable to create CSV output: %v", err)
		}

		if _, err := r.Node.SendRawTransaction(tx, true); err != nil {
			t.Fatalf("unable to broadcast transaction: %v", err)
		}

		spendableInputs[i] = csvOutput{
			RedeemScript: redeemScript,
			Utxo:         utxo,
			Timelock:     int64(timeLock),
		}
	}

	// Mine a single block including all the transactions generated above.
	if _, err := r.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Now mine 10 additional blocks giving the inputs generated above a
	// age of 11. Space out each block 10 minutes after the previous block.
	prevBlockHash, err := r.Node.GetBestBlockHash()
	if err != nil {
		t.Fatalf("unable to get prior block hash: %v", err)
	}
	prevBlock, err := r.Node.GetBlock(prevBlockHash)
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}
	for i := 0; i < relativeBlockLock; i++ {
		timeStamp := prevBlock.Header.Timestamp.Add(time.Minute * 10)
		b, err := r.GenerateAndSubmitBlock(nil, -1, timeStamp)
		if err != nil {
			t.Fatalf("unable to generate block: %v", err)
		}

		prevBlock = b.MsgBlock()
	}

	// A helper function to create fully signed transactions in-line during
	// the array initialization below.
	var inputIndex uint32
	makeTxCase := func(sequenceNum uint64, txVersion int32) *wire.MsgTx {
		csvInput := spendableInputs[inputIndex]

		tx, err := spendCSVOutput(csvInput.RedeemScript, csvInput.Utxo,
			sequenceNum, sweepOutput, txVersion)
		if err != nil {
			t.Fatalf("unable to spend CSV output: %v", err)
		}

		inputIndex++
		return tx
	}

	tests := [numTests]struct {
		tx     *wire.MsgTx
		accept bool
	}{
		// A transaction spending a single input. The
		// input has a relative time-lock of 1 block, but the disable
		// bit it set. The transaction should be rejected as a result.
		{
			tx: makeTxCase(
				blockdag.LockTimeToSequence(false, 1)|wire.SequenceLockTimeDisabled,
				1,
			),
			accept: false,
		},
		// A transaction with a single input having a 9 block
		// relative time lock. The referenced input is 11 blocks old,
		// but the CSV output requires a 10 block relative lock-time.
		// Therefore, the transaction should be rejected.
		{
			tx:     makeTxCase(blockdag.LockTimeToSequence(false, 9), 1),
			accept: false,
		},
		// A transaction with a single input having a 10 block
		// relative time lock. The referenced input is 11 blocks old so
		// the transaction should be accepted.
		{
			tx:     makeTxCase(blockdag.LockTimeToSequence(false, 10), 1),
			accept: true,
		},
		// A transaction with a single input having a 11 block
		// relative time lock. The input referenced has an input age of
		// 11 and the CSV op-code requires 10 blocks to have passed, so
		// this transaction should be accepted.
		{
			tx:     makeTxCase(blockdag.LockTimeToSequence(false, 11), 1),
			accept: true,
		},
		// A transaction whose input has a 1000 blck relative time
		// lock.  This should be rejected as the input's age is only 11
		// blocks.
		{
			tx:     makeTxCase(blockdag.LockTimeToSequence(false, 1000), 1),
			accept: false,
		},
		// A transaction with a single input having a 512,000 second
		// relative time-lock. This transaction should be rejected as 6
		// days worth of blocks haven't yet been mined. The referenced
		// input doesn't have sufficient age.
		{
			tx:     makeTxCase(blockdag.LockTimeToSequence(true, 512000), 1),
			accept: false,
		},
		// A transaction whose single input has a 512 second
		// relative time-lock. This transaction should be accepted as
		// finalized.
		{
			tx:     makeTxCase(blockdag.LockTimeToSequence(true, 512), 1),
			accept: true,
		},
	}

	for i, test := range tests {
		txid, err := r.Node.SendRawTransaction(test.tx, true)
		switch {
		// Test case passes, nothing further to report.
		case test.accept && err == nil:

		// Transaction should have been accepted but we have a non-nil
		// error.
		case test.accept && err != nil:
			t.Fatalf("test #%d, transaction should be accepted, "+
				"but was rejected: %v", i, err)

		// Transaction should have been rejected, but it was accepted.
		case !test.accept && err == nil:
			t.Fatalf("test #%d, transaction should be rejected, "+
				"but was accepted", i)

		// Transaction was rejected as wanted, nothing more to do.
		case !test.accept && err != nil:
		}

		// If the transaction should be rejected, manually mine a block
		// with the non-final transaction. It should be rejected.
		if !test.accept {
			txns := []*util.Tx{util.NewTx(test.tx)}
			_, err := r.GenerateAndSubmitBlock(txns, -1, time.Time{})
			if err == nil {
				t.Fatalf("test #%d, invalid block accepted", i)
			}

			continue
		}

		// Generate a block, the transaction should be included within
		// the newly mined block.
		blockHashes, err := r.Node.Generate(1)
		if err != nil {
			t.Fatalf("unable to mine block: %v", err)
		}
		assertTxInBlock(r, t, blockHashes[0], txid)
	}
}