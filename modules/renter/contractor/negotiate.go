package contractor

import (
	"errors"
	"net"
	"time"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

var (
	// the contractor will not form contracts above this price
	maxPrice = modules.StoragePriceToConsensus(500000) // 500k SC / TB / Month

	errTooExpensive = errors.New("host price was too high")
)

// verifySettings reads a signed HostSettings object from conn, validates the
// signature, and checks for discrepancies between the known settings and the
// received settings. If there is a discrepancy, the hostDB is notified. The
// received settings are returned.
func verifySettings(conn net.Conn, host modules.HostDBEntry, hdb hostDB) (modules.HostDBEntry, error) {
	// convert host key (types.SiaPublicKey) to a crypto.PublicKey
	if host.PublicKey.Algorithm != types.SignatureEd25519 || len(host.PublicKey.Key) != crypto.PublicKeySize {
		build.Critical("hostdb did not filter out host with wrong signature algorithm:", host.PublicKey.Algorithm)
		return modules.HostDBEntry{}, errors.New("host used unsupported signature algorithm")
	}
	var pk crypto.PublicKey
	copy(pk[:], host.PublicKey.Key)

	// read signed host settings
	var recvSettings modules.HostExternalSettings
	if err := crypto.ReadSignedObject(conn, &recvSettings, modules.MaxHostExternalSettingsLen, pk); err != nil {
		return modules.HostDBEntry{}, errors.New("couldn't read host's settings: " + err.Error())
	}
	if !recvSettings.AcceptingContracts {
		return modules.HostDBEntry{}, errors.New("host is not accepting contracts")
	}
	// TODO: check recvSettings against host.HostExternalSettings. If there is
	// a discrepancy, write the error to conn and update the hostdb
	host.HostExternalSettings = recvSettings
	return host, nil
}

// negotiateContract establishes a connection to a host and negotiates an
// initial file contract according to the terms of the host.
func negotiateContract(conn net.Conn, host modules.HostDBEntry, fc types.FileContract, txnBuilder transactionBuilder, tpool transactionPool) (Contract, error) {
	// allow 30 seconds for negotiation
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// create our key
	ourSK, ourPK, err := crypto.GenerateKeyPair()
	if err != nil {
		encoding.WriteObject(conn, "internal error")
		return Contract{}, errors.New("failed to generate keypair: " + err.Error())
	}
	ourPublicKey := types.SiaPublicKey{
		Algorithm: types.SignatureEd25519,
		Key:       ourPK[:],
	}
	// create unlock conditions
	uc := types.UnlockConditions{
		PublicKeys:         []types.SiaPublicKey{ourPublicKey, host.PublicKey},
		SignaturesRequired: 2,
	}

	// add UnlockHash to file contract
	fc.UnlockHash = uc.UnlockHash()

	// build transaction containing fc
	err = txnBuilder.FundSiacoins(fc.Payout)
	if err != nil {
		encoding.WriteObject(conn, "internal error")
		return Contract{}, err
	}
	txnBuilder.AddFileContract(fc)

	// sign the txn
	signedTxnSet, err := txnBuilder.Sign(false)
	if err != nil {
		return Contract{}, err
	}

	// calculate contract ID
	fcid := signedTxnSet[len(signedTxnSet)-1].FileContractID(0)

	// send acceptance and txn signed by us
	if err := encoding.WriteObject(conn, modules.AcceptResponse); err != nil {
		return Contract{}, errors.New("couldn't send acceptance: " + err.Error())
	}
	if err := encoding.WriteObject(conn, signedTxnSet); err != nil {
		return Contract{}, errors.New("couldn't send the contract signed by us: " + err.Error())
	}

	// read acceptance and txn signed by host
	var response string
	if err := encoding.ReadObject(conn, &response, 128); err != nil {
		return Contract{}, errors.New("couldn't read the host's response to our proposed contract: " + err.Error())
	}
	if response != modules.AcceptResponse {
		return Contract{}, errors.New("host rejected proposed contract: " + response)
	}
	var hostTxnSet []types.Transaction
	if err := encoding.ReadObject(conn, &hostTxnSet, types.BlockSizeLimit); err != nil {
		return Contract{}, errors.New("couldn't read the host's updated contract: " + err.Error())
	}

	// TODO: check txn?

	// submit to blockchain
	err = tpool.AcceptTransactionSet(hostTxnSet)
	if err == modules.ErrDuplicateTransactionSet {
		// as long as it made it into the transaction pool, we're good
		err = nil
	}
	if err != nil {
		return Contract{}, err
	}

	// create host contract
	contract := Contract{
		IP:           host.NetAddress,
		ID:           fcid,
		FileContract: fc,
		LastRevision: types.FileContractRevision{
			ParentID:              fcid,
			UnlockConditions:      uc,
			NewRevisionNumber:     fc.RevisionNumber,
			NewFileSize:           fc.FileSize,
			NewFileMerkleRoot:     fc.FileMerkleRoot,
			NewWindowStart:        fc.WindowStart,
			NewWindowEnd:          fc.WindowEnd,
			NewValidProofOutputs:  []types.SiacoinOutput{fc.ValidProofOutputs[0], fc.ValidProofOutputs[1]},
			NewMissedProofOutputs: []types.SiacoinOutput{fc.MissedProofOutputs[0], fc.MissedProofOutputs[1]},
			NewUnlockHash:         fc.UnlockHash,
		},
		LastRevisionTxn: types.Transaction{},
		SecretKey:       ourSK,
	}

	return contract, nil
}

// newContract negotiates an initial file contract with the specified host
// and returns a Contract. The contract is also saved by the HostDB.
func (c *Contractor) newContract(host modules.HostDBEntry, filesize uint64, endHeight types.BlockHeight) (Contract, error) {
	// reject hosts that are too expensive
	if host.ContractPrice.Cmp(maxPrice) > 0 {
		return Contract{}, errTooExpensive
	}

	// get an address to use for negotiation
	c.mu.Lock()
	if c.cachedAddress == (types.UnlockHash{}) {
		uc, err := c.wallet.NextAddress()
		if err != nil {
			c.mu.Unlock()
			return Contract{}, err
		}
		c.cachedAddress = uc.UnlockHash()
	}
	ourAddress := c.cachedAddress
	height := c.blockHeight
	c.mu.Unlock()
	if endHeight <= height {
		return Contract{}, errors.New("contract cannot end in the past")
	}
	duration := endHeight - height

	// create file contract
	renterCost := host.ContractPrice.Mul(types.NewCurrency64(filesize)).Mul(types.NewCurrency64(uint64(duration)))
	payout := renterCost.Add(host.Collateral)

	fc := types.FileContract{
		FileSize:       0,
		FileMerkleRoot: crypto.Hash{}, // no proof possible without data
		WindowStart:    endHeight,
		WindowEnd:      endHeight + host.WindowSize,
		Payout:         payout,
		UnlockHash:     types.UnlockHash{}, // to be filled in by negotiateContract
		RevisionNumber: 0,
		ValidProofOutputs: []types.SiacoinOutput{
			// outputs need to account for tax
			{Value: types.PostTax(height, renterCost), UnlockHash: ourAddress},
			// no collateral
			{Value: types.ZeroCurrency, UnlockHash: host.UnlockHash},
		},
		MissedProofOutputs: []types.SiacoinOutput{
			// same as above
			{Value: types.PostTax(height, renterCost), UnlockHash: ourAddress},
			// goes to the void, not the renter
			{Value: types.ZeroCurrency, UnlockHash: types.UnlockHash{}},
		},
	}

	// initiate connection
	conn, err := c.dialer.DialTimeout(host.NetAddress, 15*time.Second)
	if err != nil {
		return Contract{}, err
	}
	defer conn.Close()
	if err := encoding.WriteObject(conn, modules.RPCFormContract); err != nil {
		return Contract{}, err
	}

	// verify the host's settings and confirm its identity
	host, err = verifySettings(conn, host, c.hdb)
	if err != nil {
		return Contract{}, err
	}

	// create transaction builder
	txnBuilder := c.wallet.StartTransaction()

	// execute negotiation protocol
	contract, err := negotiateContract(conn, host, fc, txnBuilder, c.tpool)
	if err != nil {
		txnBuilder.Drop() // return unused outputs to wallet
		return Contract{}, err
	}

	c.mu.Lock()
	c.contracts[contract.ID] = contract
	c.spentPeriod = c.spentPeriod.Add(fc.Payout)
	c.spentTotal = c.spentTotal.Add(fc.Payout)
	c.cachedAddress = types.UnlockHash{} // clear the cached address
	c.save()
	c.mu.Unlock()

	return contract, nil
}

// formContracts forms contracts with hosts using the allowance parameters.
func (c *Contractor) formContracts(a modules.Allowance) error {
	// Get hosts.
	hosts := c.hdb.RandomHosts(int(2*a.Hosts), nil)
	if uint64(len(hosts)) < a.Hosts {
		return errors.New("not enough hosts")
	}
	// Calculate average host price
	var sum types.Currency
	for _, h := range hosts {
		sum = sum.Add(h.ContractPrice)
	}
	avgPrice := sum.Div(types.NewCurrency64(uint64(len(hosts))))

	// Check that allowance is sufficient to store at least one sector per
	// host for the specified duration.
	costPerSector := avgPrice.
		Mul(types.NewCurrency64(a.Hosts)).
		Mul(types.NewCurrency64(modules.SectorSize)).
		Mul(types.NewCurrency64(uint64(a.Period)))
	if a.Funds.Cmp(costPerSector) < 0 {
		return errors.New("insufficient funds")
	}

	// Calculate the filesize of the contracts by using the average host price
	// and rounding down to the nearest sector.
	numSectors, err := a.Funds.Div(costPerSector).Uint64()
	if err != nil {
		// if there was an overflow, something is definitely wrong
		return errors.New("allowance resulted in unexpectedly large contract size")
	}
	filesize := numSectors * modules.SectorSize

	// Form contracts with each host.
	c.mu.RLock()
	endHeight := c.blockHeight + a.Period
	c.mu.RUnlock()
	var numContracts uint64
	for _, h := range hosts {
		_, err := c.newContract(h, filesize, endHeight)
		if err != nil {
			// TODO: is there a better way to handle failure here? Should we
			// prefer an all-or-nothing approach? We can't pick new hosts to
			// negotiate with because they'll probably be more expensive than
			// we can afford.
			c.log.Println("WARN: failed to negotiate contract:", h.NetAddress, err)
		}
		if numContracts++; numContracts >= a.Hosts {
			break
		}
	}
	c.mu.Lock()
	c.renewHeight = endHeight
	c.mu.Unlock()
	return nil
}