package gateway

import (
	"errors"
	"io/ioutil"
	"math/rand"

	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/network"
)

func (g *Gateway) addPeer(peer network.Address) error {
	if _, exists := g.peers[peer]; exists {
		return errors.New("peer already added")
	}
	g.peers[peer] = struct{}{}
	return nil
}

func (g *Gateway) removePeer(peer network.Address) error {
	if _, exists := g.peers[peer]; !exists {
		return errors.New("no record of that peer")
	}
	delete(g.peers, peer)
	return nil
}

func (g *Gateway) randomPeer() (network.Address, error) {
	r := rand.Intn(len(g.peers))
	for peer := range g.peers {
		if r == 0 {
			return peer, nil
		}
		r--
	}
	return "", ErrNoPeers
}

func (g *Gateway) broadcast(name string, arg, resp interface{}) {
	for peer := range g.peers {
		peer.RPC(name, arg, resp)
	}
}

func (g *Gateway) save(filename string) error {
	peers := g.Info().Peers
	return ioutil.WriteFile(filename, encoding.Marshal(peers), 0666)
}

func (g *Gateway) load(filename string) (err error) {
	contents, err := ioutil.ReadFile(filename)
	if err != nil {
		return
	}
	var peers []network.Address
	err = encoding.Unmarshal(contents, &peers)
	if err != nil {
		return
	}
	for _, peer := range peers {
		g.addPeer(peer)
	}
	return
}