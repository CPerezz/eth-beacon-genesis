// Command genesis-hash prints the execution genesis block hash this build derives
// from a geth genesis.json — the single value the beacon genesis embeds as
// latest_execution_payload_header.block_hash.
//
// It exists because that derivation is the whole reason this repo needs a fork: the
// beaconchain builders call elGenesis.ToBlock(), and on an EIP-8297 chain ToBlock()
// only computes the binary-tree root if the go-ethereum this binary links against
// understands "pbt": true. Running the full beaconchain command to find that out
// needs a CL config, mnemonics and a validator set; this needs none of them.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	gethversion "github.com/ethereum/go-ethereum/version"
	"github.com/ethpandaops/eth-beacon-genesis/eth1"
)

// wantsPBT reports whether the FILE schedules the binary tree, read straight from the
// JSON rather than through params.ChainConfig. Deliberate: an unforked go-ethereum has
// no such field at all, so asking the parsed config would not compile there — and this
// probe has to build both ways for its own negative control to mean anything.
//
// The key is binaryTrieTime, a fork activation timestamp shared with besu. It used to be
// a `"pbt": true` boolean; a genesis still carrying that decodes fork-less and comes up on
// the merkle-patricia trie without complaining, so reporting what the FILE asks for is the
// whole point of this helper.
func wantsPBT(path string) (bool, *int64) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	var doc struct {
		Config struct {
			BinaryTrieTime *int64 `json:"binaryTrieTime"`
			AmsterdamTime  *int64 `json:"amsterdamTime"`
		} `json:"config"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		return false, nil
	}
	return doc.Config.BinaryTrieTime != nil, doc.Config.BinaryTrieTime
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <genesis.json>\n", os.Args[0])
		os.Exit(2)
	}

	genesis, err := eth1.LoadEth1GenesisConfig(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}

	block := genesis.ToBlock()

	fmt.Printf("go-ethereum:  v%d.%d.%d\n", gethversion.Major, gethversion.Minor, gethversion.Patch)
	fmt.Printf("chain id:     %v\n", genesis.Config.ChainID)
	scheduled, at := wantsPBT(os.Args[1])
	when := "not scheduled"
	if at != nil {
		when = fmt.Sprintf("@%d", *at)
	}
	fmt.Printf("binaryTrieTime: %v (%s)\n", scheduled, when)
	fmt.Printf("amsterdam:    %v\n", genesis.Config.AmsterdamTime != nil)
	fmt.Printf("state root:   %s\n", block.Root())
	fmt.Printf("block hash:   %s\n", block.Hash())
}
