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

package service

import (
	"context"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"github.com/whiteblock/genesis/pkg/entity"
	"github.com/whiteblock/genesis/pkg/repository"
	"github.com/whiteblock/genesis/pkg/service/auxillary"
	"strconv"
)

//DockerService provides a intermediate interface between docker and the order from a command
type DockerService interface {

	//CreateContainer attempts to create a docker container
	CreateContainer(ctx context.Context, cli *client.Client, container entity.Container) entity.Result

	//StartContainer attempts to start an already created docker container
	StartContainer(ctx context.Context, cli *client.Client, name string) entity.Result

	//RemoveContainer attempts to remove a container
	RemoveContainer(ctx context.Context, cli *client.Client, name string) entity.Result

	//CreateNetwork attempts to create a network
	CreateNetwork(ctx context.Context, cli *client.Client, net entity.Network) entity.Result

	//RemoveNetwork attempts to remove a network
	RemoveNetwork(ctx context.Context, cli *client.Client, name string) entity.Result
	AttachNetwork(ctx context.Context, cli *client.Client, network string, container string) entity.Result
	CreateVolume(ctx context.Context, cli *client.Client, volume entity.Volume) entity.Result
	RemoveVolume(ctx context.Context, cli *client.Client, name string) entity.Result
	PlaceFileInContainer(ctx context.Context, cli *client.Client, containerName string, file entity.File) entity.Result
	PlaceFileInVolume(ctx context.Context, cli *client.Client, volumeName string, file entity.File) entity.Result
	Emulation(ctx context.Context, cli *client.Client, netem entity.Netconf) entity.Result

	//CreateClient creates a new client for connecting to the docker daemon
	CreateClient(conf entity.DockerConfig, host string) (*client.Client, error)

	//GetNetworkingConfig determines the proper networking config based on the docker hosts state and the networks
	GetNetworkingConfig(ctx context.Context, cli *client.Client, networks strslice.StrSlice) (*network.NetworkingConfig, error)
}

type dockerService struct {
	repo repository.DockerRepository
	aux  auxillary.DockerAuxillary
}

//NewDockerService creates a new DockerService
func NewDockerService(repo repository.DockerRepository, aux auxillary.DockerAuxillary) (DockerService, error) {
	return dockerService{repo: repo, aux: aux}, nil
}

//CreateClient creates a new client for connecting to the docker daemon
func (ds dockerService) CreateClient(conf entity.DockerConfig, host string) (*client.Client, error) {
	if conf.LocalMode {
		return client.NewClientWithOpts(
			client.WithAPIVersionNegotiation(),
		)
	}
	return client.NewClientWithOpts(
		client.WithAPIVersionNegotiation(),
		client.WithHost(host),
		client.WithTLSClientConfig(conf.CACertPath, conf.CertPath, conf.KeyPath),
	)
}

//GetNetworkingConfig determines the proper networking config based on the docker hosts state and the networks
func (ds dockerService) GetNetworkingConfig(ctx context.Context, cli *client.Client,
	networks strslice.StrSlice) (*network.NetworkingConfig, error) {

	resourceChan := make(chan types.NetworkResource, len(networks))
	errChan := make(chan error, len(networks))

	for _, net := range networks {
		go func(net string) {
			resource, err := ds.aux.GetNetworkByName(ctx, cli, net)
			errChan <- err
			resourceChan <- resource
		}(net)
	}
	out := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	for range networks {
		err := <-errChan
		if err != nil {
			return out, err
		}
		resource := <-resourceChan
		out.EndpointsConfig[resource.Name] = &network.EndpointSettings{
			NetworkID: resource.ID,
		}
	}
	return out, nil
}

//CreateContainer attempts to create a docker container
func (ds dockerService) CreateContainer(ctx context.Context, cli *client.Client, dContainer entity.Container) entity.Result {
	errChan := make(chan error)
	netConfChan := make(chan *network.NetworkingConfig)
	defer close(errChan)
	defer close(netConfChan)

	go func(image string) {
		errChan <- ds.aux.EnsureImagePulled(ctx, cli, image)
	}(dContainer.Image)

	go func(networks strslice.StrSlice) {
		networkConfig, err := ds.GetNetworkingConfig(ctx, cli, networks)
		netConfChan <- networkConfig
		errChan <- err
	}(dContainer.Network)

	portSet, portMap, err := dContainer.GetPortBindings()
	if err != nil {
		return entity.NewFatalResult(err)
	}

	config := &container.Config{
		Hostname:     dContainer.Name,
		Domainname:   dContainer.Name,
		ExposedPorts: portSet,
		Env:          dContainer.GetEnv(),
		Image:        dContainer.Image,
		Entrypoint:   dContainer.GetEntryPoint(),
		Labels:       dContainer.Labels,
	}

	mem, err := dContainer.GetMemory()
	if err != nil {
		return entity.NewFatalResult(err)
	}

	cpus, err := strconv.ParseFloat(dContainer.Cpus, 64)
	if err != nil {
		return entity.NewFatalResult(err)
	}

	hostConfig := &container.HostConfig{
		PortBindings: portMap,
		AutoRemove:   true,
	}
	hostConfig.NanoCPUs = int64(1000000000 * cpus)
	hostConfig.Memory = mem

	networkConfig := <-netConfChan

	for i := 0; i < 2; i++ {
		err = <-errChan
		if err != nil {
			return entity.NewErrorResult(err)
		}
	}

	_, err = ds.repo.ContainerCreate(ctx, cli, config, hostConfig, networkConfig, dContainer.Name)
	if err != nil {
		return entity.NewFatalResult(err)
	}

	return entity.NewSuccessResult()
}

//StartContainer attempts to start an already created docker container
func (ds dockerService) StartContainer(ctx context.Context, cli *client.Client, name string) entity.Result {
	log.WithFields(log.Fields{"name": name}).Trace("starting container")
	opts := types.ContainerStartOptions{}
	err := ds.repo.ContainerStart(ctx, cli, name, opts)
	if err != nil {
		return entity.NewErrorResult(err)
	}

	return entity.NewSuccessResult()
}

//RemoveContainer attempts to remove a container
func (ds dockerService) RemoveContainer(ctx context.Context, cli *client.Client, name string) entity.Result {
	cntr, err := ds.aux.GetContainerByName(ctx, cli, name)
	if err != nil {
		return entity.NewErrorResult(err)
	}
	err = ds.repo.ContainerRemove(ctx, cli, cntr.ID, types.ContainerRemoveOptions{
		RemoveVolumes: false,
		RemoveLinks:   false,
		Force:         true,
	})
	if err != nil {
		return entity.NewErrorResult(err)
	}
	return entity.NewSuccessResult()
}

//CreateNetwork attempts to create a network
func (ds dockerService) CreateNetwork(ctx context.Context, cli *client.Client, net entity.Network) entity.Result {
	networkCreate := types.NetworkCreate{
		CheckDuplicate: true,
		Attachable:     true,
		Ingress:        false,
		Internal:       false,
		Labels:         net.Labels,
		IPAM: &network.IPAM{
			Driver:  "default",
			Options: nil,
			Config: []network.IPAMConfig{
				network.IPAMConfig{
					Subnet:  net.Subnet,
					Gateway: net.Gateway,
				},
			},
		},
		Options: map[string]string{},
	}
	if net.Global {
		networkCreate.Driver = "overlay"
		networkCreate.Scope = "swarm"
	} else {
		networkCreate.Driver = "bridge"
		networkCreate.Scope = "local"
		networkCreate.Options["com.docker.network.bridge.name"] = net.Name
	}
	_, err := ds.repo.NetworkCreate(ctx, cli, net.Name, networkCreate)
	if err != nil {
		return entity.NewErrorResult(err)
	}
	return entity.NewSuccessResult()
}

//RemoveNetwork attempts to remove a network
func (ds dockerService) RemoveNetwork(ctx context.Context, cli *client.Client, name string) entity.Result {
	net, err := ds.aux.GetNetworkByName(ctx, cli, name)
	if err != nil {
		return entity.NewErrorResult(err)
	}
	err = ds.repo.NetworkRemove(ctx, cli, net.ID)
	if err != nil {
		return entity.NewErrorResult(err)
	}
	return entity.NewSuccessResult()
}

func (ds dockerService) AttachNetwork(ctx context.Context, cli *client.Client, network string,
	containerName string) entity.Result {
	//TODO
	return entity.Result{}
}

func (ds dockerService) CreateVolume(ctx context.Context, cli *client.Client, vol entity.Volume) entity.Result {
	volConfig := volume.VolumeCreateBody{
		Driver:     vol.Driver,
		DriverOpts: vol.DriverOpts,
		Labels:     vol.Labels,
		Name:       vol.Name,
	}

	_, err := ds.repo.VolumeCreate(ctx, cli, volConfig)
	if err != nil {
		return entity.NewErrorResult(err)
	}

	return entity.NewSuccessResult()
}

func (ds dockerService) RemoveVolume(ctx context.Context, cli *client.Client, name string) entity.Result {
	//TODO
	return entity.Result{}
}

func (ds dockerService) PlaceFileInContainer(ctx context.Context, cli *client.Client, containerName string, file entity.File) entity.Result {
	//TODO
	return entity.Result{}
}

func (ds dockerService) PlaceFileInVolume(ctx context.Context, cli *client.Client, volumeName string, file entity.File) entity.Result {
	//TODO
	return entity.Result{}
}

func (ds dockerService) Emulation(ctx context.Context, cli *client.Client, netem entity.Netconf) entity.Result {
	//TODO
	return entity.Result{}
}
