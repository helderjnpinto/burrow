#!/usr/bin/env bash

script_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )/chain/genesis.json"
echo $script_dir

address_of() {
    jq -r ".Accounts | map(select(.Name == \"$1\"))[0].Address" $script_dir
}

full_addr=$(address_of "Full_0")

../bin/burrow keys export --addr ${full_addr} '--template={address: "<< .Address >>", pubKey: "<< hex .PublicKey  >>", privKey: "<< hex .PrivateKey >>" }'
