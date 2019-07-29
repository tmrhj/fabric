/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package lifecycle

import (
	"strings"

	"github.com/hyperledger/fabric/common/util"
	corechaincode "github.com/hyperledger/fabric/core/chaincode"
	persistence "github.com/hyperledger/fabric/core/chaincode/persistence/intf"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/scc"

	"github.com/pkg/errors"
)

//go:generate counterfeiter -o mock/legacy_lifecycle.go --fake-name LegacyLifecycle . LegacyLifecycle
type LegacyLifecycle interface {
	corechaincode.Lifecycle
}

//go:generate counterfeiter -o mock/chaincode_info_cache.go --fake-name ChaincodeInfoCache . ChaincodeInfoCache
type ChaincodeInfoCache interface {
	ChaincodeInfo(channelID, chaincodeName string) (definition *LocalChaincodeInfo, err error)
}

// LegacyDefinition is an implmentor of ccprovider.ChaincodeDefinition.
// It is a different data-type to allow differentiation at cast-time from
// chaincode definitions which require validaiton of instantiation policy.
type LegacyDefinition struct {
	Name              string
	Version           string
	HashField         []byte
	EndorsementPlugin string
	RequiresInitField bool
}

// CCName returns the chaincode name
func (ld *LegacyDefinition) CCName() string {
	return ld.Name
}

// Hash returns the hash of <name>:<version>.  This is useless, but
// is a hack to allow the rest of the code to have consistent view of
// what hash means for a chaincode definition.  Ultimately, this should
// be removed.
func (ld *LegacyDefinition) Hash() []byte {
	return util.ComputeSHA256([]byte(ld.Name + ":" + ld.Version))
}

// CCVersion returns the version of the chaincode.
func (ld *LegacyDefinition) CCVersion() string {
	return ld.Version
}

// Endorsement returns how to endorse proposals for this chaincode.
// The string returns is the name of the endorsement method (usually 'escc').
func (ld *LegacyDefinition) Endorsement() string {
	return ld.EndorsementPlugin
}

// RequiresInit returns whether this chaincode must have Init commit before invoking.
func (ld *LegacyDefinition) RequiresInit() bool {
	return ld.RequiresInitField
}

type ChaincodeEndorsementInfo struct {
	Resources    *Resources
	Cache        ChaincodeInfoCache
	LegacyImpl   LegacyLifecycle
	BuiltinSCCs  scc.BuiltinSCCs
	SysCCVersion string
}

func (cei *ChaincodeEndorsementInfo) CachedChaincodeInfo(channelID, chaincodeName string, qe ledger.SimpleQueryExecutor) (*LocalChaincodeInfo, bool, error) {
	var qes ReadableState = &SimpleQueryExecutorShim{
		Namespace:           LifecycleNamespace,
		SimpleQueryExecutor: qe,
	}

	if qe == nil {
		// NOTE: the core/chaincode package inconsistently sets the
		// query executor depending on whether the call has a channel
		// context or not. We use this dummy shim which always returns
		// an error for GetState calls to avoid a peer panic.
		qes = &DummyQueryExecutorShim{}
	}

	currentSequence, err := cei.Resources.Serializer.DeserializeFieldAsInt64(NamespacesName, chaincodeName, "Sequence", qes)
	if err != nil {
		return nil, false, errors.WithMessagef(err, "could not get current sequence for chaincode '%s' on channel '%s'", chaincodeName, channelID)
	}

	// Committed sequences begin at 1
	if currentSequence == 0 {
		return nil, false, nil
	}

	chaincodeInfo, err := cei.Cache.ChaincodeInfo(channelID, chaincodeName)
	if err != nil {
		return nil, false, errors.WithMessage(err, "could not get approved chaincode info from cache")
	}

	if chaincodeInfo.Definition.Sequence != currentSequence {
		// TODO this is a transient error which indicates that this query executor is executing against a chaincode
		// whose definition has already changed (the cache may be ahead of the committed state, but never behind).  In this
		// case, we should simply abort the tx, and re-acquire a query executor and re-execute.  There is no reason this
		// error needs to be returned to the client.
		return nil, false, errors.Errorf("chaincode cache at sequence %d but current sequence is %d, chaincode definition for '%s' changed during invoke", chaincodeInfo.Definition.Sequence, currentSequence, chaincodeName)
	}

	if !chaincodeInfo.Approved {
		return nil, false, errors.Errorf("chaincode definition for '%s' at sequence %d on channel '%s' has not yet been approved by this org", chaincodeName, currentSequence, channelID)
	}

	if chaincodeInfo.InstallInfo == nil {
		return nil, false, errors.Errorf("chaincode definition for '%s' exists, but chaincode is not installed", chaincodeName)
	}

	return chaincodeInfo, true, nil

}

// ChaincodeDefinition returns the details for a chaincode by name
func (cei *ChaincodeEndorsementInfo) ChaincodeDefinition(channelID, chaincodeName string, qe ledger.SimpleQueryExecutor) (ccprovider.ChaincodeDefinition, error) {
	chaincodeInfo, ok, err := cei.CachedChaincodeInfo(channelID, chaincodeName, qe)
	if err != nil {
		return nil, err
	}
	if !ok {
		return cei.LegacyImpl.ChaincodeDefinition(channelID, chaincodeName, qe)
	}

	chaincodeDefinition := chaincodeInfo.Definition

	return &LegacyDefinition{
		Name:              chaincodeName,
		Version:           chaincodeDefinition.EndorsementInfo.Version,
		EndorsementPlugin: chaincodeDefinition.EndorsementInfo.EndorsementPlugin,
		RequiresInitField: chaincodeDefinition.EndorsementInfo.InitRequired,
	}, nil
}

// ChaincodeContainerInfo returns the information necessary to launch a chaincode, it also returns
// static definitions for the fabric defined system chaincodes
func (cei *ChaincodeEndorsementInfo) ChaincodeContainerInfo(channelID, chaincodeName string, qe ledger.SimpleQueryExecutor) (*ccprovider.ChaincodeContainerInfo, error) {
	if cei.BuiltinSCCs.IsSysCC(chaincodeName) {
		return &ccprovider.ChaincodeContainerInfo{
			PackageID: persistence.PackageID(chaincodeName + ":" + cei.SysCCVersion),
			Name:      chaincodeName,
			Version:   cei.SysCCVersion,
		}, nil
	}

	chaincodeInfo, ok, err := cei.CachedChaincodeInfo(channelID, chaincodeName, qe)
	if err != nil {
		return nil, err
	}
	if !ok {
		return cei.LegacyImpl.ChaincodeContainerInfo(channelID, chaincodeName, qe)
	}

	return &ccprovider.ChaincodeContainerInfo{
		Name:      chaincodeName,
		Version:   chaincodeInfo.Definition.EndorsementInfo.Version,
		Path:      chaincodeInfo.InstallInfo.Path,
		Type:      strings.ToUpper(chaincodeInfo.InstallInfo.Type),
		PackageID: chaincodeInfo.InstallInfo.PackageID,
	}, nil
}
