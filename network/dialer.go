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

package network

import (
	"context"
	"net"
	"time"
)

// Dialer establish tcp-level connection with the destination
type Dialer struct {
	phonebook   *MultiPhonebook
	innerDialer net.Dialer
}

// makeRateLimitingDialer creates a rate limiting dialer that would limit the connections
// according to the entries in the phonebook.
func makeRateLimitingDialer(phonebook *MultiPhonebook) Dialer {
	return Dialer{
		phonebook: phonebook,
		innerDialer: net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		},
	}
}

// Dial connects to the address on the named network.
// It waits if needed not to exceed connectionsRateLimitingCount.
func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// DialContext connects to the address on the named network using the provided context.
// It waits if needed not to exceed connectionsRateLimitingCount.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var waitTime time.Duration
	var provisionalTime time.Time

	for {
		_, waitTime, provisionalTime = d.phonebook.GetConnectionWaitTime(address)
		if waitTime == 0 {
			break // break out of the loop and proceed to the connection
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitTime):
		}
	}
	conn, err := d.innerDialContext(ctx, network, address)
	d.phonebook.UpdateConnectionTime(address, provisionalTime)

	return conn, err
}

func (d *Dialer) innerDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// this would be a good place to have the dnssec evaluated.
	// TODO: replace d.innerDialer.Resolver with dnssec.DefaultResolver
	// Possible alternatives: add a config option EnableDNSSEC or make a fall-back resolver
	return d.innerDialer.DialContext(ctx, network, address)
}
