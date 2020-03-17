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

package basics

import (
	"github.com/algorand/go-algorand/config"
)

// DeltaAction is an enum of actions that may be performed when applying a
// delta to a TEAL key/value store
type DeltaAction uint64

const (
	// SetUintAction indicates that a Uint should be stored at a key
	SetUintAction DeltaAction = 1

	// SetBytesAction indicates that a TEAL byte slice should be stored at a key
	SetBytesAction DeltaAction = 2

	// DeleteAction indicates that the value for a particular key should be deleted
	DeleteAction DeltaAction = 3
)

// ValueDelta links a DeltaAction with a value to be set
type ValueDelta struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	Action DeltaAction `codec:"at"`
	Bytes  string      `codec:"bs"`
	Uint   uint64      `codec:"ui"`
}

// StateDelta is a map from key/value store keys to ValueDeltas, indicating
// what should happen for that key
//msgp:allocbound StateDelta -
type StateDelta map[string]ValueDelta

// EvalDelta stores StateDeltas for an application's global key/value store, as
// well as StateDeltas for some number of accounts holding local state for that
// application
type EvalDelta struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	GlobalDelta StateDelta `codec:"gd"`

	// TODO(applications) perhaps make these keys be uint64 where 0 == sender
	// and 1..n -> txn.Addresses
	LocalDeltas map[Address]StateDelta `codec:"ld,allocbound=-"`
}

// MakeEvalDelta creates an EvalDelta and allocates its fields
func MakeEvalDelta() EvalDelta {
	return EvalDelta{
		GlobalDelta: make(StateDelta),
		LocalDeltas: make(map[Address]StateDelta),
	}
}

// StateSchema sets maximums on the number of each value type that may be stored
type StateSchema struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	NumUint      uint64 `codec:"nui"`
	NumByteSlice uint64 `codec:"nbs"`
}

// NumEntries counts the total number of values that may be stored for particular schema
func (sm StateSchema) NumEntries() (tot uint64) {
	tot = AddSaturate(tot, sm.NumUint)
	tot = AddSaturate(tot, sm.NumByteSlice)
	return tot
}

// MinBalance computes the MinBalance requirements for a StateSchema based on
// the consensus parameters
func (sm StateSchema) MinBalance(proto config.ConsensusParams) (res MicroAlgos) {
	// Flat cost for each key/value pair
	flatCost := MulSaturate(proto.SchemaMinBalancePerEntry, sm.NumEntries())

	// Cost for uints
	uintCost := MulSaturate(proto.SchemaUintMinBalance, sm.NumUint)

	// Cost for byte slices
	bytesCost := MulSaturate(proto.SchemaBytesMinBalance, sm.NumByteSlice)

	// Sum the separate costs
	var min uint64
	min = AddSaturate(min, flatCost)
	min = AddSaturate(min, uintCost)
	min = AddSaturate(min, bytesCost)

	res.Raw = min
	return res
}

// TealType is an enum of the types in a TEAL program: Bytes and Uint
type TealType uint64

const (
	// TealBytesType represents the type of a byte slice in a TEAL program
	TealBytesType TealType = 1

	// TealUintType represents the type of a uint in a TEAL program
	TealUintType TealType = 2
)

// TealValue contains type information and a value, representing a value in a
// TEAL program
type TealValue struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	Type TealType `codec:"tt"`

	Bytes string `codec:"tb"`
	Uint  uint64 `codec:"ui"`
}

// ToValueDelta creates ValueDelta from TealValue
func (tv *TealValue) ToValueDelta() (vd ValueDelta) {
	if tv.Type == TealUintType {
		vd.Action = SetUintAction
		vd.Uint = tv.Uint
	} else {
		vd.Action = SetBytesAction
		vd.Bytes = tv.Bytes
	}
	return
}

// TealKeyValue represents a key/value store for use in an application's
// LocalState or GlobalState
//msgp:allocbound TealKeyValue 4096
type TealKeyValue map[string]TealValue

// Clone returns a copy of a TealKeyValue that may be modified without
// affecting the original
func (tk TealKeyValue) Clone() TealKeyValue {
	if tk == nil {
		return nil
	}
	res := make(TealKeyValue, len(tk))
	for k, v := range tk {
		res[k] = v
	}
	return res
}

// SatisfiesSchema returns a boolean indicating whether or not a particular
// TealKeyValue store meets the requirements set by a StateSchema on how
// many values of each type are allowed
func (tk TealKeyValue) SatisfiesSchema(schema StateSchema) bool {
	// Count all of the types in the key/value store
	var uintCount, bytesCount uint64
	for _, value := range tk {
		switch value.Type {
		case TealBytesType:
			bytesCount++
		case TealUintType:
			uintCount++
		default:
			// Shouldn't happen
			return false
		}
	}

	// Check against the schema
	if uintCount > schema.NumUint {
		return false
	}
	if bytesCount > schema.NumByteSlice {
		return false
	}
	return true
}
