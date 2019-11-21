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

package handler

import (
	"encoding/json"
	"testing"

	"github.com/streadway/amqp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	usecaseMocks "github.com/whiteblock/genesis/mocks/pkg/usecase"
	"github.com/whiteblock/genesis/pkg/command"
	"github.com/whiteblock/genesis/pkg/entity"
)

func TestNewDeliveryHandler(t *testing.T) {
	assert.NotNil(t, NewDeliveryHandler(nil, nil, nil))
}

func TestDeliveryHandler_Process_Successful(t *testing.T) {
	duc := new(usecaseMocks.DockerUseCase)
	duc.On("Run", mock.Anything).Return(entity.Result{Type: entity.SuccessType}).Once()

	dh := NewDeliveryHandler(duc)

	cmd := command.Command{
		Order: command.Order{
			Type:    "createContainer",
			Payload: map[string]interface{}{},
		},
		Target: command.Target{
			IP: "127.0.0.1",
		},
	}

	body, err := json.Marshal(cmd)
	if err != nil {
		t.Error(err)
	}

	pub, res := dh.Process(amqp.Delivery{Body: body})
	assert.NoError(t, res.Error)

	duc.AssertExpectations(t)
}

func TestDeliveryHandler_Process_Unsuccessful(t *testing.T) {
	dh := NewDeliveryHandler(new(usecaseMocks.DockerUseCase))

	body := []byte("should be a failure")

	res := dh.ProcessMessage(amqp.Delivery{Body: body})
	assert.Error(t, res.Error)
}
