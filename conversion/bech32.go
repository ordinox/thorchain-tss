package conversion

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func SetupBech32Prefix() {
	config := sdk.GetConfig()
	// thorchain will import go-tss as a library , thus this is not needed, we copy the prefix here to avoid go-tss to import thorchain
	config.SetBech32PrefixForAccount("ordinox", "thorpub")
	config.SetBech32PrefixForValidator("ordinox", "thorvpub")
	config.SetBech32PrefixForConsensusNode("or", "thorcpub")

}
