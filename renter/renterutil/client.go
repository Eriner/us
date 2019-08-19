package renterutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/node/api/client"
	"gitlab.com/NebulousLabs/Sia/types"
	"lukechampine.com/us/hostdb"
	"lukechampine.com/us/renter"
	"lukechampine.com/us/renter/proto"
	"lukechampine.com/us/wallet"
	"lukechampine.com/walrus"
)

// ErrNoHostAnnouncement is returned when a host announcement cannot be found.
var ErrNoHostAnnouncement = errors.New("host announcement not found")

// SiadClient wraps the siad API client. It satisfies the proto.Wallet,
// proto.TransactionPool, and renter.HostKeyResolver interfaces. The
// proto.Wallet methods require that the wallet is unlocked.
type SiadClient struct {
	siad *client.Client
}

// ChainHeight returns the current block height.
func (c *SiadClient) ChainHeight() (types.BlockHeight, error) {
	cg, err := c.siad.ConsensusGet()
	return cg.Height, err
}

// Synced returns whether the siad node believes it is fully synchronized with
// the rest of the network.
func (c *SiadClient) Synced() (bool, error) {
	cg, err := c.siad.ConsensusGet()
	return cg.Synced, err
}

// AcceptTransactionSet submits a transaction set to the transaction pool,
// where it will be broadcast to other peers.
func (c *SiadClient) AcceptTransactionSet(txnSet []types.Transaction) error {
	if len(txnSet) == 0 {
		return errors.New("empty transaction set")
	}
	txn, parents := txnSet[len(txnSet)-1], txnSet[:len(txnSet)-1]
	return c.siad.TransactionPoolRawPost(txn, parents)
}

// FeeEstimate returns the current estimate for transaction fees, in Hastings
// per byte.
func (c *SiadClient) FeeEstimate() (minFee, maxFee types.Currency, err error) {
	tfg, err := c.siad.TransactionPoolFeeGet()
	return tfg.Minimum, tfg.Maximum, err
}

// NewWalletAddress returns a new address generated by the wallet.
func (c *SiadClient) NewWalletAddress() (types.UnlockHash, error) {
	wag, err := c.siad.WalletAddressGet()
	return wag.Address, err
}

// SignTransaction adds the specified signatures to the transaction using
// private keys known to the wallet.
func (c *SiadClient) SignTransaction(txn *types.Transaction, toSign []crypto.Hash) error {
	wspr, err := c.siad.WalletSignPost(*txn, toSign)
	if err == nil {
		*txn = wspr.Transaction
	}
	return err
}

// UnspentOutputs returns the set of outputs tracked by the wallet that are
// spendable.
func (c *SiadClient) UnspentOutputs() ([]modules.UnspentOutput, error) {
	wug, err := c.siad.WalletUnspentGet()
	return wug.Outputs, err
}

// UnconfirmedParents returns any currently-unconfirmed parents of the specified
// transaction.
func (c *SiadClient) UnconfirmedParents(txn types.Transaction) ([]types.Transaction, error) {
	return nil, nil // not supported
}

// UnlockConditions returns the UnlockConditions that correspond to the
// specified address.
func (c *SiadClient) UnlockConditions(addr types.UnlockHash) (types.UnlockConditions, error) {
	wucg, err := c.siad.WalletUnlockConditionsGet(addr)
	return wucg.UnlockConditions, err
}

// HostDB

// LookupHost returns the host public key matching the specified prefix.
func (c *SiadClient) LookupHost(prefix string) (hostdb.HostPublicKey, error) {
	if !strings.HasPrefix(prefix, "ed25519:") {
		prefix = "ed25519:" + prefix
	}
	hdag, err := c.siad.HostDbAllGet()
	if err != nil {
		return "", err
	}
	var hpk hostdb.HostPublicKey
	for i := range hdag.Hosts {
		key := hostdb.HostPublicKey(hdag.Hosts[i].PublicKeyString)
		if strings.HasPrefix(string(key), prefix) {
			if hpk != "" {
				return "", errors.New("ambiguous pubkey")
			}
			hpk = key
		}
	}
	if hpk == "" {
		return "", errors.New("no host with that pubkey")
	}
	return hpk, nil
}

// ResolveHostKey resolves a host public key to that host's most recently
// announced network address.
func (c *SiadClient) ResolveHostKey(pubkey hostdb.HostPublicKey) (modules.NetAddress, error) {
	hhg, err := c.siad.HostDbHostsGet(pubkey.SiaPublicKey())
	if err != nil && strings.Contains(err.Error(), "requested host does not exist") {
		return "", ErrNoHostAnnouncement
	}
	return hhg.Entry.NetAddress, err
}

// NewSiadClient returns a SiadClient that communicates with the siad API
// server at the specified address.
func NewSiadClient(addr, password string) *SiadClient {
	c := client.New(addr)
	c.Password = password
	return &SiadClient{siad: c}
}

// A SHARDClient communicates with a SHARD server. It satisfies the
// renter.HostKeyResolver interface.
type SHARDClient struct {
	addr string
}

func (c *SHARDClient) req(route string, fn func(*http.Response) error) error {
	resp, err := http.Get(fmt.Sprintf("http://%v%v", c.addr, route))
	if err != nil {
		return err
	}
	defer io.Copy(ioutil.Discard, resp.Body)
	defer resp.Body.Close()

	if !(200 <= resp.StatusCode && resp.StatusCode <= 299) {
		errString, _ := ioutil.ReadAll(resp.Body)
		return errors.New(string(errString))
	}
	return fn(resp)
}

// ChainHeight returns the current block height.
func (c *SHARDClient) ChainHeight() (types.BlockHeight, error) {
	var height types.BlockHeight
	err := c.req("/height", func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&height)
	})
	return height, err
}

// Synced returns whether the SHARD server is synced.
func (c *SHARDClient) Synced() (bool, error) {
	var synced bool
	err := c.req("/synced", func(resp *http.Response) error {
		data, err := ioutil.ReadAll(io.LimitReader(resp.Body, 8))
		if err != nil {
			return err
		}
		synced, err = strconv.ParseBool(string(data))
		return err
	})
	return synced, err
}

// ResolveHostKey resolves a host public key to that host's most recently
// announced network address.
func (c *SHARDClient) ResolveHostKey(pubkey hostdb.HostPublicKey) (modules.NetAddress, error) {
	var ha modules.HostAnnouncement
	var sig crypto.Signature
	err := c.req("/host/"+string(pubkey), func(resp *http.Response) error {
		if resp.StatusCode == http.StatusNoContent {
			return ErrNoHostAnnouncement
		} else if resp.StatusCode == http.StatusGone {
			return errors.New("ambiguous pubkey")
		}
		return encoding.NewDecoder(resp.Body, encoding.DefaultAllocLimit).DecodeAll(&ha, &sig)
	})
	if err != nil {
		return "", err
	}

	// verify signature
	if !pubkey.VerifyHash(crypto.HashObject(ha), sig[:]) {
		return "", errors.New("invalid signature")
	}

	return ha.NetAddress, err
}

// LookupHost returns the host public key matching the specified prefix.
func (c *SHARDClient) LookupHost(prefix string) (hostdb.HostPublicKey, error) {
	var ha modules.HostAnnouncement
	var sig crypto.Signature
	err := c.req("/host/"+prefix, func(resp *http.Response) error {
		if resp.ContentLength == 0 {
			return ErrNoHostAnnouncement
		}
		return encoding.NewDecoder(resp.Body, encoding.DefaultAllocLimit).DecodeAll(&ha, &sig)
	})
	if err != nil {
		return "", err
	}
	return hostdb.HostKeyFromSiaPublicKey(ha.PublicKey), nil
}

// NewSHARDClient returns a SHARDClient that communicates with the SHARD
// server at the specified address.
func NewSHARDClient(addr string) *SHARDClient {
	return &SHARDClient{addr: addr}
}

// WalrusClient wraps the walrus API. It satisfies the proto.Wallet and
// proto.TransactionPool interfaces using a walrus server and an in-memory seed.
type WalrusClient struct {
	walrus *walrus.Client
	seed   wallet.Seed
}

// AcceptTransactionSet submits a transaction set to the transaction pool,
// where it will be broadcast to other peers.
func (c *WalrusClient) AcceptTransactionSet(txnSet []types.Transaction) error {
	return c.walrus.Broadcast(txnSet)
}

// FeeEstimate returns the current estimate for transaction fees, in Hastings
// per byte.
func (c *WalrusClient) FeeEstimate() (minFee, maxFee types.Currency, err error) {
	fee, err := c.walrus.RecommendedFee()
	return fee, fee.Mul64(3), err
}

// NewWalletAddress returns a new address generated by the wallet.
func (c *WalrusClient) NewWalletAddress() (types.UnlockHash, error) {
	index, err := c.walrus.SeedIndex()
	if err != nil {
		return types.UnlockHash{}, err
	}
	info := wallet.SeedAddressInfo{
		UnlockConditions: wallet.StandardUnlockConditions(c.seed.PublicKey(index)),
		KeyIndex:         index,
	}
	if err := c.walrus.AddAddress(info); err != nil {
		return types.UnlockHash{}, err
	}
	return info.UnlockHash(), nil
}

// SignTransaction adds the specified signatures to the transaction using
// private keys known to the wallet.
func (c *WalrusClient) SignTransaction(txn *types.Transaction, toSign []crypto.Hash) error {
	if len(toSign) == 0 {
		// lazy mode: add standard sigs for every input we own
		for _, input := range txn.SiacoinInputs {
			info, err := c.walrus.AddressInfo(input.UnlockConditions.UnlockHash())
			if err != nil {
				// TODO: catch errors other than "address not found"
				continue
			}
			sk := c.seed.SecretKey(info.KeyIndex)
			txnSig := wallet.StandardTransactionSignature(crypto.Hash(input.ParentID))
			wallet.AppendTransactionSignature(txn, txnSig, sk)
		}
		return nil
	}

	sigAddr := func(id crypto.Hash) (types.UnlockHash, bool) {
		for _, sci := range txn.SiacoinInputs {
			if crypto.Hash(sci.ParentID) == id {
				return sci.UnlockConditions.UnlockHash(), true
			}
		}
		for _, sfi := range txn.SiafundInputs {
			if crypto.Hash(sfi.ParentID) == id {
				return sfi.UnlockConditions.UnlockHash(), true
			}
		}
		for _, fcr := range txn.FileContractRevisions {
			if crypto.Hash(fcr.ParentID) == id {
				return fcr.UnlockConditions.UnlockHash(), true
			}
		}
		return types.UnlockHash{}, false
	}
	sign := func(i int) error {
		addr, ok := sigAddr(txn.TransactionSignatures[i].ParentID)
		if !ok {
			return errors.New("invalid id")
		}
		info, err := c.walrus.AddressInfo(addr)
		if err != nil {
			return err
		}
		sk := c.seed.SecretKey(info.KeyIndex)
		txn.TransactionSignatures[i].Signature = sk.SignHash(txn.SigHash(i, types.ASICHardforkHeight+1))
		return nil
	}

outer:
	for _, parent := range toSign {
		for sigIndex, sig := range txn.TransactionSignatures {
			if sig.ParentID == parent {
				if err := sign(sigIndex); err != nil {
					return err
				}
				continue outer
			}
		}
		return errors.New("sighash not found in transaction")
	}

	return nil
}

// UnspentOutputs returns the set of outputs tracked by the wallet that are
// spendable.
func (c *WalrusClient) UnspentOutputs() ([]modules.UnspentOutput, error) {
	utxos, err := c.walrus.UnspentOutputs(false)
	outputs := make([]modules.UnspentOutput, len(utxos))
	for i := range outputs {
		outputs[i] = modules.UnspentOutput{
			FundType:   types.SpecifierSiacoinOutput,
			ID:         types.OutputID(utxos[i].ID),
			UnlockHash: utxos[i].UnlockHash,
			Value:      utxos[i].Value,
		}
	}
	return outputs, err
}

// UnconfirmedParents returns any currently-unconfirmed parents of the specified
// transaction.
func (c *WalrusClient) UnconfirmedParents(txn types.Transaction) ([]types.Transaction, error) {
	limboParents, err := c.walrus.UnconfirmedParents(txn)
	parents := make([]types.Transaction, len(limboParents))
	for i := range parents {
		parents[i] = limboParents[i].Transaction
	}
	return parents, err
}

// UnlockConditions returns the UnlockConditions that correspond to the
// specified address.
func (c *WalrusClient) UnlockConditions(addr types.UnlockHash) (types.UnlockConditions, error) {
	info, err := c.walrus.AddressInfo(addr)
	return info.UnlockConditions, err
}

// NewWalrusClient returns a WalrusClient using the specified server address and
// seed.
func NewWalrusClient(addr string, seed wallet.Seed) *WalrusClient {
	return &WalrusClient{
		walrus: walrus.NewClient(addr),
		seed:   seed,
	}
}

// verify that clients satisfy their intended interfaces
var (
	_ interface {
		proto.Wallet
		proto.TransactionPool
		renter.HostKeyResolver
	} = (*SiadClient)(nil)
	_ interface {
		proto.Wallet
		proto.TransactionPool
	} = (*WalrusClient)(nil)
	_ renter.HostKeyResolver = (*SHARDClient)(nil)
)
