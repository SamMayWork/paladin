/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package zetosigner

import (
	"fmt"
	"math/big"

	"github.com/iden3/go-iden3-crypto/poseidon"
)

func CalculateNullifier(value, salt *big.Int, privateKeyForZkp *big.Int) (*big.Int, error) {
	nullifier, err := poseidon.Hash([]*big.Int{value, salt, privateKeyForZkp})
	if err != nil {
		return nil, fmt.Errorf("failed to create the nullifier hash. %s", err)
	}
	return nullifier, nil
}
