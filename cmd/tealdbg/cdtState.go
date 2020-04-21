// Copyright (C) 2019-2020 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/spec/v1"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
)

type cdtState struct {
	// immutable content
	program    string
	proto      *config.ConsensusParams
	txnGroup   []transactions.SignedTxn
	groupIndex int

	// mutable program state
	mu      deadlock.Mutex
	stack   []v1.TealValue
	scratch []v1.TealValue
	pc      atomicInt
	line    atomicInt
	err     atomicString

	// debugger states
	lastAction      atomicString
	pauseOnError    atomicBool
	pauseOnCompeted atomicBool
	completed       atomicBool
}

func (s *cdtState) Init(program string, proto *config.ConsensusParams, txnGroup []transactions.SignedTxn, groupIndex int) {
	s.program = program
	s.proto = proto
	s.txnGroup = txnGroup
	s.groupIndex = groupIndex
}

func (s *cdtState) Update(pc int, line int, stack []v1.TealValue, scratch []v1.TealValue, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pc.Store(pc)
	s.line.Store(line)
	s.stack = stack
	s.scratch = scratch
	s.err.Store(err)
}

const localScopeObjID = "localScopeObjId"
const globalScopeObjID = "globalScopeObjID"
const globalsObjID = "globalsObjID"
const txnObjID = "txnObjID"
const gtxnObjID = "gtxnObjID"
const stackObjID = "stackObjID"
const scratchObjID = "scratchObjID"
const tealErrorID = "tealErrorID"

type objectDescFn func(s *cdtState, preview bool) []RuntimePropertyDescriptor

var objectDescMap = map[string]objectDescFn{
	globalScopeObjID: makeGlobalScope,
	localScopeObjID:  makeLocalScope,
	globalsObjID:     makeGlobals,
	txnObjID:         makeTxn,
	gtxnObjID:        makeTxnGroup,
	stackObjID:       makeStack,
	scratchObjID:     makeScratch,
	tealErrorID:      makeTealError,
}

func (s *cdtState) getObjectDescriptor(objID string, preview bool) (descr []RuntimePropertyDescriptor, err error) {
	maker, ok := objectDescMap[objID]
	if !ok {
		if idx, ok := decodeGroupTxnID(objID); ok {
			if idx >= len(s.txnGroup) || idx < 0 {
				err = fmt.Errorf("invalid group idx: %d", idx)
				return
			}
			if len(s.txnGroup) > 0 {
				return makeTxnImpl(&s.txnGroup[idx].Txn, idx, preview), nil
			}
		}
		// might be nested object in array, parse and call
		err = fmt.Errorf("unk object id: %s", objID)
		return
	}
	return maker(s, preview), nil
}

func makeObject(name, id string) RuntimePropertyDescriptor {
	return RuntimePropertyDescriptor{
		Name:         name,
		Configurable: false,
		Writable:     false,
		Enumerable:   true,
		IsOwn:        true,
		Value: &RuntimeRemoteObject{
			Type:        "object",
			ClassName:   "Object",
			Description: "Object",
			ObjectID:    id,
		},
	}
}

func makeArray(name string, length int, id string) RuntimePropertyDescriptor {
	return RuntimePropertyDescriptor{
		Name:         name,
		Configurable: false,
		Writable:     false,
		Enumerable:   true,
		IsOwn:        true,
		Value: &RuntimeRemoteObject{
			Type:        "object",
			Subtype:     "array",
			ClassName:   "Array",
			Description: fmt.Sprintf("Array(%d)", length),
			ObjectID:    id,
		},
	}
}

func makePrimitive(field fieldDesc) RuntimePropertyDescriptor {
	return RuntimePropertyDescriptor{
		Name:         field.Name,
		Configurable: false,
		Writable:     false,
		Enumerable:   true,
		IsOwn:        true,
		Value: &RuntimeRemoteObject{
			Type:  field.Type,
			Value: field.Value,
		},
	}
}

// tealTypeMap maps TealType to JS type
var tealTypeMap = map[basics.TealType]string{
	basics.TealBytesType: "string",
	basics.TealUintType:  "number",
}

type fieldDesc struct {
	Name  string
	Value string
	Type  string
}

func prepareGlobals(txnGroup []transactions.SignedTxn, groupIndex int, proto *config.ConsensusParams) []fieldDesc {
	result := make([]fieldDesc, 0, len(logic.GlobalFieldNames))
	for _, name := range logic.GlobalFieldNames {
		var value string
		var valType string = "string"
		tv, err := logic.GlobalFieldToTealValue(proto, txnGroup, groupIndex)
		if err != nil {
			value = err.Error()
			valType = "undefined"
		} else {
			value = tv.String()
			valType = tealTypeMap[tv.Type]
		}
		result = append(result, fieldDesc{name, value, valType})
	}
	return result
}

func prepareTxn(txn *transactions.Transaction, groupIndex int) []fieldDesc {
	result := make([]fieldDesc, 0, len(logic.TxnFieldNames))
	for field, name := range logic.TxnFieldNames {
		if field == int(logic.FirstValidTime) ||
			field == int(logic.Accounts) ||
			field == int(logic.ApplicationArgs) {
			continue
		}
		var value string
		var valType string = "string"
		tv, err := logic.TxnFieldToTealValue(txn, groupIndex, logic.TxnField(field))
		if err != nil {
			value = err.Error()
			valType = "undefined"
		} else {
			value = tv.String()
			valType = tealTypeMap[tv.Type]
		}
		result = append(result, fieldDesc{name, value, valType})
	}
	return result
}

func prepareArray(array []v1.TealValue) []fieldDesc {
	result := make([]fieldDesc, 0, len(logic.TxnFieldNames))
	for i := 0; i < len(array); i++ {
		tv := array[i]
		name := strconv.Itoa(i)
		var value string
		var valType string
		if tv.Type == "b" {
			valType = "string"
			data, err := base64.StdEncoding.DecodeString(tv.Bytes)
			if err != nil {
				value = tv.Bytes
			} else {
				printable := true
				for i := 0; i < len(data); i++ {
					if !strconv.IsPrint(rune(data[i])) {
						printable = false
						break
					}
				}
				if printable {
					value = string(data)
				} else if len(data) < 8 {
					value = fmt.Sprintf("%q", data)
					if value[0] == '"' {
						value = value[1 : len(value)-1]
					}
				} else {
					value = hex.EncodeToString(data)
				}
			}
		} else {
			valType = "number"
			value = strconv.Itoa(int(tv.Uint))
		}
		result = append(result, fieldDesc{name, value, valType})
	}
	return result
}

func makeTxnPreview(txnGroup []transactions.SignedTxn, groupIndex int) RuntimeObjectPreview {
	var prop []RuntimePropertyPreview
	if len(txnGroup) > 0 {
		fields := prepareTxn(&txnGroup[groupIndex].Txn, groupIndex)
		for _, field := range fields {
			v := RuntimePropertyPreview{
				Name:  field.Name,
				Value: field.Value,
				Type:  field.Type,
			}
			prop = append(prop, v)
		}
	}

	p := RuntimeObjectPreview{Type: "object", Overflow: true, Properties: prop}
	return p
}

func makeGtxnPreview(txnGroup []transactions.SignedTxn) RuntimeObjectPreview {
	var prop []RuntimePropertyPreview
	if len(txnGroup) > 0 {
		for i := 0; i < len(txnGroup); i++ {
			v := RuntimePropertyPreview{
				Name:  strconv.Itoa(i),
				Value: "Object",
				Type:  "object",
			}
			prop = append(prop, v)
		}
	}
	p := RuntimeObjectPreview{
		Type:        "object",
		Subtype:     "array",
		Description: fmt.Sprintf("Array(%d)", len(txnGroup)),
		Overflow:    false,
		Properties:  prop}
	return p
}

const maxArrayPreviewLength = 20

func makeArrayPreview(array []v1.TealValue) RuntimeObjectPreview {
	var prop []RuntimePropertyPreview
	fields := prepareArray(array)

	length := len(fields)
	if length > maxArrayPreviewLength {
		length = maxArrayPreviewLength
	}
	for _, field := range fields[:length] {
		v := RuntimePropertyPreview{
			Name:  field.Name,
			Value: field.Value,
			Type:  field.Type,
		}
		prop = append(prop, v)
	}

	p := RuntimeObjectPreview{
		Type:        "object",
		Subtype:     "array",
		Description: fmt.Sprintf("Array(%d)", len(array)),
		Overflow:    true,
		Properties:  prop}
	return p
}

func makeGlobalsPreview(txnGroup []transactions.SignedTxn, groupIndex int, proto *config.ConsensusParams) RuntimeObjectPreview {
	var prop []RuntimePropertyPreview
	fields := prepareGlobals(txnGroup, groupIndex, proto)

	for _, field := range fields {
		v := RuntimePropertyPreview{
			Name:  field.Name,
			Value: field.Value,
			Type:  field.Type,
		}
		prop = append(prop, v)
	}

	p := RuntimeObjectPreview{
		Type:        "object",
		Description: "Object",
		Overflow:    true,
		Properties:  prop}
	return p
}

var gtxnObjIDPrefix = fmt.Sprintf("%s_gid_", gtxnObjID)

func encodeGroupTxnID(groupIndex int) string {
	return gtxnObjIDPrefix + strconv.Itoa(groupIndex)
}

func decodeGroupTxnID(objID string) (int, bool) {
	if strings.HasPrefix(objID, gtxnObjIDPrefix) {
		if val, err := strconv.ParseInt(objID[len(gtxnObjIDPrefix):], 10, 32); err == nil {
			return int(val), true
		}
	}
	return 0, false
}

func makeGlobalScope(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	globals := makeObject("globals", globalsObjID)
	if preview {
		globalsPreview := makeGlobalsPreview(s.txnGroup, s.groupIndex, s.proto)
		globals.Value.Preview = &globalsPreview
	}

	descr = []RuntimePropertyDescriptor{
		globals,
	}
	return descr
}

func makeLocalScope(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	txn := makeObject("txn", txnObjID)
	gtxn := makeArray("gtxn", len(s.txnGroup), gtxnObjID)
	stack := makeArray("stack", len(s.stack), stackObjID)
	scratch := makeArray("scratch", len(s.scratch), scratchObjID)
	if preview {
		txnPreview := makeTxnPreview(s.txnGroup, s.groupIndex)
		if len(txnPreview.Properties) > 0 {
			txn.Value.Preview = &txnPreview
		}
		gtxnPreview := makeGtxnPreview(s.txnGroup)
		if len(gtxnPreview.Properties) > 0 {
			gtxn.Value.Preview = &gtxnPreview
		}
		stackPreview := makeArrayPreview(s.stack)
		if len(stackPreview.Properties) > 0 {
			stack.Value.Preview = &stackPreview
		}
		scratchPreview := makeArrayPreview(s.scratch)
		if len(scratchPreview.Properties) > 0 {
			scratch.Value.Preview = &scratchPreview
		}
	}

	pc := makePrimitive(fieldDesc{
		Name:  "PC",
		Value: strconv.Itoa(s.line.Load()),
		Type:  "number",
	})
	descr = []RuntimePropertyDescriptor{
		pc,
		txn,
		gtxn,
		stack,
		scratch,
	}

	return descr
}

func makeGlobals(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	fields := prepareGlobals(s.txnGroup, s.groupIndex, s.proto)
	for _, field := range fields {
		descr = append(descr, makePrimitive(field))
	}
	return
}

func makeTxn(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	if len(s.txnGroup) > 0 && s.groupIndex < len(s.txnGroup) && s.groupIndex >= 0 {
		return makeTxnImpl(&s.txnGroup[s.groupIndex].Txn, s.groupIndex, preview)
	}
	return
}

func makeTxnImpl(txn *transactions.Transaction, groupIndex int, preview bool) (descr []RuntimePropertyDescriptor) {
	fields := prepareTxn(txn, groupIndex)
	for _, field := range fields {
		descr = append(descr, makePrimitive(field))
	}
	return
}

func makeTxnGroup(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	if len(s.txnGroup) > 0 {
		for i := 0; i < len(s.txnGroup); i++ {
			item := makeObject(strconv.Itoa(i), encodeGroupTxnID(i))
			if preview {
				txnPreview := makeTxnPreview(s.txnGroup, i)
				item.Value.Preview = &txnPreview
			}
			descr = append(descr, item)
		}
	}
	return
}

func makeStack(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	fmt.Printf("makeStack\n")
	stack := make([]v1.TealValue, len(s.stack))
	for i := 0; i < len(stack); i++ {
		stack[i] = s.stack[len(s.stack)-1-i]
	}

	fields := prepareArray(stack)
	for _, field := range fields {
		descr = append(descr, makePrimitive(field))
	}
	field := fieldDesc{Name: "length", Value: strconv.Itoa(len(s.stack)), Type: "number"}
	descr = append(descr, makePrimitive(field))
	return
}

func makeScratch(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	fields := prepareArray(s.scratch)
	for _, field := range fields {
		descr = append(descr, makePrimitive(field))
	}
	field := fieldDesc{Name: "length", Value: strconv.Itoa(len(s.scratch)), Type: "number"}
	descr = append(descr, makePrimitive(field))
	return
}

func makeTealError(s *cdtState, preview bool) (descr []RuntimePropertyDescriptor) {
	if lastError := s.err.Load(); len(lastError) != 0 {
		field := fieldDesc{Name: "message", Value: lastError, Type: "string"}
		descr = append(descr, makePrimitive(field))
	}
	return
}