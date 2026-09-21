// Package messagingcontract binds broker call sites to AsyncAPI operations.
package messagingcontract

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

type Binder struct{}

func New() *Binder                        { return &Binder{} }
func (b *Binder) Name() string            { return "messaging-contract" }
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

type contractRef struct {
	identity, operationID, file, protocol, content string
}

type binding struct {
	contract contractRef
	symbol   string
}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()
	rawContracts := map[string][]contractRef{}
	contracts := map[string][]contractRef{}
	duplicateOf := map[string][]string{}
	canonicalOf := map[string]string{}
	for _, f := range all {
		if !isContractOperation(f) {
			continue
		}
		key := operationKey(f)
		rawContracts[key] = append(rawContracts[key], contractRef{
			identity: contractIdentity(f), operationID: f.PropString("operationId"), file: f.File,
			protocol: f.PropString(facts.PropMessaging), content: contractContentKey(f),
		})
	}
	// Cluster equivalent declarations first, then compare clusters. This makes
	// conflict metadata independent of traversal order (A, conflicting B, copy A).
	for key, refs := range rawContracts {
		groups := map[string][]contractRef{}
		var groupOrder []string
		for _, ref := range refs {
			groupKey := ref.protocol + "\x00" + ref.content
			if _, ok := groups[groupKey]; !ok {
				groupOrder = append(groupOrder, groupKey)
			}
			groups[groupKey] = append(groups[groupKey], ref)
		}
		for _, groupKey := range groupOrder {
			members := groups[groupKey]
			canonical := members[0]
			contracts[key] = append(contracts[key], canonical)
			for _, member := range members {
				canonicalOf[member.identity] = canonical.identity
				for _, otherKey := range groupOrder {
					if otherKey == groupKey {
						continue
					}
					for _, other := range groups[otherKey] {
						duplicateOf[member.identity] = append(duplicateOf[member.identity], other.file)
					}
				}
			}
		}
	}

	bindings := map[string]binding{}
	statuses := map[string]string{}
	candidateCounts := map[string]int{}
	implementers := map[string]map[string]bool{}
	implementations := map[string]map[string]bool{}
	for _, f := range all {
		if !isCodeOperation(f) {
			continue
		}
		identity := codeIdentity(f)
		candidates := contracts[operationKey(f)]
		matches := compatibleContracts(candidates, f.PropString(facts.PropMessaging))
		candidateCounts[identity] = len(matches)
		switch {
		case len(candidates) == 0:
			statuses[identity] = facts.MessagingContractStatusUndeclared
		case len(matches) == 0:
			statuses[identity] = facts.MessagingContractStatusProtocolMismatch
		case len(matches) > 1:
			statuses[identity] = facts.MessagingContractStatusAmbiguous
		default:
			statuses[identity] = facts.MessagingContractStatusBound
		}
		if statuses[identity] != facts.MessagingContractStatusBound {
			continue
		}
		symbol := f.PropString("code_symbol")
		bindings[identity] = binding{contract: matches[0], symbol: symbol}
		if implementations[matches[0].identity] == nil {
			implementations[matches[0].identity] = map[string]bool{}
		}
		implementationKey := "call:" + identity
		if symbol != "" {
			implementationKey = "symbol:" + symbol
		}
		implementations[matches[0].identity][implementationKey] = true
		if symbol != "" {
			if implementers[matches[0].identity] == nil {
				implementers[matches[0].identity] = map[string]bool{}
			}
			implementers[matches[0].identity][symbol] = true
		}
	}

	bound := 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindStorage || f.Props == nil {
			return
		}
		if isCodeOperation(*f) {
			f.DelProp(facts.PropMessagingContractBound)
			f.DelProp(facts.PropMessagingContractOperationID)
			f.DelProp(facts.PropMessagingContractFile)
			identity := codeIdentity(*f)
			f.SetProp(facts.PropMessagingContractStatus, statuses[identity])
			f.SetProp(facts.PropMessagingContractCandidates, candidateCounts[identity])
			if match, ok := bindings[identity]; ok {
				f.SetProp(facts.PropMessagingContractBound, true)
				if match.contract.operationID != "" {
					f.SetProp(facts.PropMessagingContractOperationID, match.contract.operationID)
				}
				f.SetProp(facts.PropMessagingContractFile, match.contract.file)
				bound++
			}
			return
		}
		if !isContractOperation(*f) {
			return
		}
		f.DelProp(facts.PropMessagingImplementationCount)
		f.DelProp(facts.PropMessagingImplementedBy)
		f.DelProp(facts.PropMessagingImplementationStatus)
		f.DelProp(facts.PropMessagingDuplicateOf)
		f.DelProp(facts.PropMessagingCanonicalFile)
		kept := f.Relations[:0]
		for _, relation := range f.Relations {
			if relation.Kind != facts.RelImplementedBy {
				kept = append(kept, relation)
			}
		}
		f.Relations = kept
		identity := contractIdentity(*f)
		if dup := dedupSorted(duplicateOf[identity]); len(dup) > 0 {
			f.SetProp(facts.PropMessagingDuplicateOf, dup)
		}
		canonical := canonicalOf[identity]
		if canonical != "" && canonical != identity {
			for _, ref := range contracts[operationKey(*f)] {
				if ref.identity == canonical {
					f.SetProp(facts.PropMessagingCanonicalFile, ref.file)
					break
				}
			}
		}
		set := implementers[canonical]
		implementationCount := len(implementations[canonical])
		f.SetProp(facts.PropMessagingImplementationCount, implementationCount)
		if implementationCount == 0 {
			f.SetProp(facts.PropMessagingImplementationStatus, facts.MessagingImplementationUnimplemented)
			return
		}
		f.SetProp(facts.PropMessagingImplementationStatus, facts.MessagingImplementationImplemented)
		symbols := make([]string, 0, len(set))
		for symbol := range set {
			symbols = append(symbols, symbol)
		}
		sort.Strings(symbols)
		f.SetProp(facts.PropMessagingImplementedBy, symbols)
		for _, symbol := range symbols {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplementedBy, Target: symbol})
		}
	})
	if bound > 0 {
		log.Printf("[binder:messaging-contract] bound %d messaging call site(s) to AsyncAPI operations", bound)
	}
	return nil
}

func compatibleContracts(candidates []contractRef, codeProtocol string) []contractRef {
	out := make([]contractRef, 0, len(candidates))
	for _, candidate := range candidates {
		if messagingProtocolsCompatible(codeProtocol, candidate.protocol) {
			out = append(out, candidate)
		}
	}
	return out
}

// messagingProtocolsCompatible permits a missing protocol for backward
// compatibility with contracts whose server is unspecified, and treats Kafka's
// secure variant as the same broker family. Explicitly different technologies
// must never bind merely because their channel name and direction coincide.
func messagingProtocolsCompatible(codeProtocol, contractProtocol string) bool {
	codeProtocol = strings.TrimSpace(codeProtocol)
	contractProtocol = strings.TrimSpace(contractProtocol)
	if codeProtocol == "" || contractProtocol == "" {
		return true
	}
	return facts.MessagingProtocolFamily(codeProtocol) == facts.MessagingProtocolFamily(contractProtocol)
}

func isContractOperation(f facts.Fact) bool {
	return f.Kind == facts.KindStorage && f.PropString("storage_kind") == facts.StorageKindTopic &&
		f.PropString(facts.PropSource) == facts.MessagingSourceAsyncAPI &&
		f.PropString(facts.PropMessagingOperation) != ""
}

func isCodeOperation(f facts.Fact) bool {
	return f.Kind == facts.KindStorage && f.PropString("storage_kind") == facts.StorageKindTopic &&
		facts.IsMessagingCodeSource(f.PropString(facts.PropSource)) &&
		f.PropString(facts.PropMessagingOperation) != ""
}

func operationKey(f facts.Fact) string {
	return f.Repo + "\x00" + f.Name + "\x00" + f.PropString(facts.PropMessagingOperation)
}

func contractIdentity(f facts.Fact) string {
	return operationKey(f) + "\x00" + f.File + "\x00" + f.PropString("operationId")
}

func codeIdentity(f facts.Fact) string {
	return operationKey(f) + "\x00" + f.File + "\x00" + strconv.Itoa(f.Line) + "\x00" + f.PropString("code_symbol")
}

func contractContentKey(f facts.Fact) string {
	props := make(map[string]any, f.PropCount())
	for key, value := range f.Props {
		switch key {
		case "spec_file", facts.PropMessagingContractBound, facts.PropMessagingContractOperationID,
			facts.PropMessagingContractFile, facts.PropMessagingImplementationCount,
			facts.PropMessagingImplementedBy, facts.PropMessagingContractStatus,
			facts.PropMessagingContractCandidates, facts.PropMessagingImplementationStatus,
			facts.PropMessagingDuplicateOf, facts.PropMessagingCanonicalFile:
			continue
		}
		props[key] = value
	}
	encoded, _ := json.Marshal(props) // extractor props are JSON-compatible
	return string(encoded)
}

func dedupSorted(files []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for _, file := range files {
		if !seen[file] {
			seen[file] = true
			out = append(out, file)
		}
	}
	sort.Strings(out)
	return out
}
