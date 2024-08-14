// Copyright © 2024 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sequence

import (
	"strconv"

	"github.com/serialx/hashring"
)

func NewContentionResolver() ContentionResolver {
	return &contentionResolver{}
}

type ContentionResolver interface {
	Resolve(stateHash, biddingContentionResolver1, biddingContentionResolver2 string) (string, error)
}

type contentionResolver struct {
}

func (c *contentionResolver) Resolve(stateHash, bidder1, bidder2 string) (string, error) {

	bidders := make([]string, 0, 1000)
	// create 500 virtual nodes for each bidding ContentionResolver
	for i := 0; i < 500; i++ {
		bidders = append(bidders, bidder1+strconv.Itoa(i))
		bidders = append(bidders, bidder2+strconv.Itoa(i))
	}
	ring := hashring.New(bidders)
	winnerVirtual, _ := ring.GetNode(stateHash)
	winner := winnerVirtual[:36]
	return winner, nil

}
