package proto

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"time"

	"github.com/lukechampine/us/hostdb"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
	"github.com/pkg/errors"
)

// isHostDisconnect reports whether err was caused by the host closing the
// connection.
func isHostDisconnect(err error) bool {
	// NOTE: this is unfortunately the best we can do; the Go standard library
	// does the same. See golang/go@fb4b4342
	return err != nil && strings.Contains(err.Error(), "use of closed network connection")
}

// extendDeadline is a helper function for extending the connection timeout.
func extendDeadline(conn net.Conn, d time.Duration) { _ = conn.SetDeadline(time.Now().Add(d)) }

func initiateRPC(addr modules.NetAddress, rpc types.Specifier, contract ContractEditor) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", string(addr), 15*time.Second)
	if err != nil {
		return nil, errors.Wrap(err, "could not dial host")
	}
	// allot 2 minutes for RPC request + revision exchange
	extendDeadline(conn, modules.NegotiateRecentRevisionTime)
	defer extendDeadline(conn, time.Hour)
	if err := encoding.WriteObject(conn, rpc); err != nil {
		conn.Close()
		return nil, errors.Wrap(err, "could not initiate RPC")
	}
	hostRev, hostSigs, err := verifyRecentRevision(conn, contract.Transaction())
	if err != nil {
		conn.Close()
		return nil, errors.Wrap(err, "could not verify most recent contract revision")
	} else if err := contract.SyncWithHost(hostRev, hostSigs); err != nil {
		conn.Close() // TODO: close gracefully
		return nil, errors.Wrap(err, "could not synchronize contract with host")
	}
	return conn, nil
}

func terminateRPC(conn net.Conn, host hostdb.ScannedHost) error {
	extendDeadline(conn, modules.NegotiateSettingsTime)
	// don't care about these errors
	_, _ = verifySettings(conn, host)
	_ = modules.WriteNegotiationStop(conn)
	return conn.Close()
}

// startRevision is run at the beginning of each revision iteration. It reads
// the host's settings, confirms that the values are acceptable, and writes an
// acceptance. If the values are acceptable, host is updated.
func startRevision(conn net.Conn, host *hostdb.ScannedHost) error {
	// verify the host's settings and confirm its identity
	newhost, err := verifySettings(conn, *host)
	if err != nil {
		return err
	}
	// return an error if any of the host's prices increased
	// TODO: let user specify limits
	if newhost.UploadBandwidthPrice.Cmp(host.UploadBandwidthPrice) > 0 {
		return errors.New("host upload price increased")
	} else if newhost.DownloadBandwidthPrice.Cmp(host.DownloadBandwidthPrice) > 0 {
		return errors.New("host download price increased")
	} else if newhost.StoragePrice.Cmp(host.StoragePrice) > 0 {
		return errors.New("host storage price increased")
	}
	*host = newhost
	return modules.WriteNegotiationAcceptance(conn)
}

// verifySettings reads a signed HostSettings object from conn, validates the
// signature using the host's pubkey, and returns a Host with the received
// settings.
func verifySettings(conn net.Conn, host hostdb.ScannedHost) (hostdb.ScannedHost, error) {
	// read signed host settings
	var recvSettings hostdb.HostSettings
	if err := crypto.ReadSignedObject(conn, &recvSettings, modules.NegotiateMaxHostExternalSettingsLen, host.PublicKey.Ed25519()); err != nil {
		return hostdb.ScannedHost{}, errors.Wrap(err, "could not read signed host settings")
	}
	if recvSettings.NetAddress != host.NetAddress {
		// for now, just overwrite the NetAddress, since we know that
		// host.NetAddress works (it was the one we dialed to get conn)
		recvSettings.NetAddress = host.NetAddress
	}
	host.HostSettings = recvSettings
	return host, nil
}

// verifyRecentRevision confirms that the host and contractor agree upon the current
// state of the contract being revised.
func verifyRecentRevision(conn net.Conn, contract ContractTransaction) (types.FileContractRevision, []types.TransactionSignature, error) {
	// send contract ID
	if err := encoding.WriteObject(conn, contract.ID()); err != nil {
		return types.FileContractRevision{}, nil, errors.Wrap(err, "could not send contract ID")
	}
	// read challenge
	var challenge crypto.Hash
	if err := encoding.ReadObject(conn, &challenge, 32); err != nil {
		return types.FileContractRevision{}, nil, errors.Wrap(err, "could not read challenge")
	}
	crypto.SecureWipe(challenge[:16])
	// sign and return
	sig := crypto.SignHash(challenge, contract.RenterKey)
	if err := encoding.WriteObject(conn, sig); err != nil {
		return types.FileContractRevision{}, nil, errors.Wrap(err, "could not send challenge response")
	}
	// read acceptance
	if err := modules.ReadNegotiationAcceptance(conn); err != nil {
		return types.FileContractRevision{}, nil, errors.Wrap(err, "host did not accept revision request")
	}
	// read host revision and signatures
	var hostRevision types.FileContractRevision
	var hostSignatures []types.TransactionSignature
	if err := encoding.ReadObject(conn, &hostRevision, 2048); err != nil {
		return types.FileContractRevision{}, nil, errors.Wrap(err, "could not read host revision")
	}
	if err := encoding.ReadObject(conn, &hostSignatures, 2048); err != nil {
		return types.FileContractRevision{}, nil, errors.Wrap(err, "could not read host signatures")
	}

	// validate the transaction signatures
	// NOTE: we can fake the blockheight here because it doesn't affect
	// verification; it just needs to be above the fork height and below the
	// contract expiration (which was checked earlier).
	err := modules.VerifyFileContractRevisionTransactionSignatures(hostRevision, hostSignatures, contract.EndHeight()-1)
	if err != nil {
		return types.FileContractRevision{}, nil, errors.Wrap(err, "host sent invalid transaction")
	}
	// Check that the unlock hashes match; if they do not, something is
	// seriously wrong.
	if hostRevision.UnlockConditions.UnlockHash() != contract.CurrentRevision().UnlockConditions.UnlockHash() {
		return types.FileContractRevision{}, nil, errors.New("unlock conditions do not match")
	}
	return hostRevision, hostSignatures, nil
}

// negotiateRevision sends a revision and actions to the host for approval,
// completing one iteration of the revision loop.
func negotiateRevision(conn net.Conn, rev types.FileContractRevision, secretKey crypto.SecretKey) (types.Transaction, error) {
	// create transaction containing the revision
	signedTxn := types.Transaction{
		FileContractRevisions: []types.FileContractRevision{rev},
		TransactionSignatures: []types.TransactionSignature{{
			ParentID:       crypto.Hash(rev.ParentID),
			CoveredFields:  types.CoveredFields{FileContractRevisions: []uint64{0}},
			PublicKeyIndex: 0, // renter key is always first -- see formContract
		}},
	}
	// sign the transaction
	encodedSig := crypto.SignHash(signedTxn.SigHash(0), secretKey)
	signedTxn.TransactionSignatures[0].Signature = encodedSig[:]

	// send the revision
	if err := encoding.WriteObject(conn, rev); err != nil {
		return types.Transaction{}, errors.Wrap(err, "could not send revision")
	}
	// read acceptance
	if err := modules.ReadNegotiationAcceptance(conn); err != nil {
		return types.Transaction{}, errors.Wrap(err, "host did not accept revision")
	}

	// send the new transaction signature
	if err := encoding.WriteObject(conn, signedTxn.TransactionSignatures[0]); err != nil {
		return types.Transaction{}, errors.Wrap(err, "could not send transaction signature")
	}
	// read the host's acceptance and transaction signature
	// NOTE: if the host sends ErrStopResponse, we should continue processing
	// the revision, but return the error anyway.
	responseErr := modules.ReadNegotiationAcceptance(conn)
	if responseErr != nil && responseErr != modules.ErrStopResponse {
		return types.Transaction{}, errors.Wrap(responseErr, "host did not accept transaction signature")
	}
	var hostSig types.TransactionSignature
	if err := encoding.ReadObject(conn, &hostSig, 16e3); err != nil {
		return types.Transaction{}, errors.Wrap(err, "could not read host's signature")
	}

	// add the signature to the transaction and verify it
	// NOTE: we can fake the blockheight here because it doesn't affect
	// verification; it just needs to be above the fork height and below the
	// contract expiration (which was checked earlier).
	verificationHeight := rev.NewWindowStart - 1
	signedTxn.TransactionSignatures = append(signedTxn.TransactionSignatures, hostSig)
	if err := signedTxn.StandaloneValid(verificationHeight); err != nil {
		return types.Transaction{}, errors.Wrap(err, "negotiated transaction is invalid")
	}

	// if the host sent ErrStopResponse, return it
	return signedTxn, responseErr
}

// newRevision creates a copy of current with its revision number incremented,
// and with cost transferred from the renter to the host.
func newRevision(current types.FileContractRevision, cost types.Currency) types.FileContractRevision {
	rev := current

	// need to manually copy slice memory
	rev.NewValidProofOutputs = make([]types.SiacoinOutput, 2)
	rev.NewMissedProofOutputs = make([]types.SiacoinOutput, 3)
	copy(rev.NewValidProofOutputs, current.NewValidProofOutputs)
	copy(rev.NewMissedProofOutputs, current.NewMissedProofOutputs)

	// move valid payout from renter to host
	rev.NewValidProofOutputs[0].Value = current.NewValidProofOutputs[0].Value.Sub(cost)
	rev.NewValidProofOutputs[1].Value = current.NewValidProofOutputs[1].Value.Add(cost)

	// move missed payout from renter to void
	rev.NewMissedProofOutputs[0].Value = current.NewMissedProofOutputs[0].Value.Sub(cost)
	rev.NewMissedProofOutputs[2].Value = current.NewMissedProofOutputs[2].Value.Add(cost)

	// increment revision number
	rev.NewRevisionNumber++

	return rev
}

// newDownloadRevision revises the current revision to cover the cost of
// downloading data.
func newDownloadRevision(current types.FileContractRevision, downloadCost types.Currency) types.FileContractRevision {
	return newRevision(current, downloadCost)
}

// newUploadRevision revises the current revision to cover the cost of
// uploading a sector.
func newUploadRevision(current types.FileContractRevision, merkleRoot crypto.Hash, price, collateral types.Currency) types.FileContractRevision {
	rev := newRevision(current, price)

	// move collateral from host to void
	rev.NewMissedProofOutputs[1].Value = rev.NewMissedProofOutputs[1].Value.Sub(collateral)
	rev.NewMissedProofOutputs[2].Value = rev.NewMissedProofOutputs[2].Value.Add(collateral)

	// set new filesize and Merkle root
	rev.NewFileSize += SectorSize
	rev.NewFileMerkleRoot = merkleRoot
	return rev
}

type actionSet []modules.RevisionAction

func (s actionSet) MarshalSia(w io.Writer) error {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(len(s)))
	w.Write(buf)
	for _, action := range s {
		w.Write(action.Type[:])
		binary.LittleEndian.PutUint64(buf, action.SectorIndex)
		w.Write(buf)
		binary.LittleEndian.PutUint64(buf, action.Offset)
		w.Write(buf)
		binary.LittleEndian.PutUint64(buf, uint64(len(action.Data)))
		w.Write(buf)
		if _, err := w.Write(action.Data); err != nil {
			return err
		}
	}
	return nil
}

func (s actionSet) MarshalSiaSize() (size int) {
	size += 8 // slice length prefix
	for _, action := range s {
		size += types.SpecifierLen
		size += 8 // SectorIndex
		size += 8 // Offset
		size += 8 // len(action.Data)
		size += len(action.Data)
	}
	return
}
