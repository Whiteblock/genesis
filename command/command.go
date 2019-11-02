/*
	Copyright 2019 whiteblock Inc.
	This file is a part of the genesis.

	Genesis is free software: you can redistribute it and/or modify
    it under the terms of the GNU General Public License as published by
    the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    Genesis is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU General Public License for more details.

    You should have received a copy of the GNU General Public License
    along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package command

// Order to be executed by genesis
type Order struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// Target sets the target of a command - which testnet, instance to hit
type Target struct {
	Testnet string `json:"testnet"`
}

// Command is the command sent to Genesis.
type Command struct {
	ID           string   `json:"id"`
	Timestamp    int64    `json:"timestamp"`
	Retry        uint8    `json:"retry"`
	Target       Target   `json:"target"`
	Dependencies []string `json:"dependencies"`
	Order        Order    `json:"order"`
}