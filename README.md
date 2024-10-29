# TON Payment Network

This is an implementation of a peer-to-peer payment network with multi-node routing based on the power of the TON Blockchain. More powerful than lightning!

## Technical Description

The network consists of peer-to-peer standalone payment nodes that communicate with each other via smart contract deployment to the blockchain and over the network using RLDP.

A payment node can be either a standalone service if the main goal is to make money from serving virtual channel chains, or part of other applications (as a library) if the goal is to provide or pay for services, such as TON Storage and TON Proxy.

Example interactions: 
![Untitled Diagram drawio(3)](https://github.com/xssnick/ton-payment-network/assets/9332353/c127d64f-2f04-4e70-87e6-252d08d1ce47)


### Onchain channels

Each node is sure to monitor new blocks in the network and catch updates related to its contracts. For example, the appearance of new contracts with her key, when someone wants to establish a channel, and events related to uncoordinated closures.

If a node wants to establish a link with another node, it deploys a payment channel contract to the blockchain. The contract contains 2 public keys: its own and its neighbor's. The other node detects the contract in the network, checks the parameters, and if all is well, allows to establish a network connection with itself.

For authentication over the network, channel keys are used, a special authentication message is formed from the adnl addresses of the parties and a timestamp and signed with the channel key, the response message must contain the adnl addresses reversed, the timestamp and the signature of the other party.

### Virtual channels

A virtual channel can be opened from any point of the network to any other if there is a chain of links between nodes, including onchain contract and active network connection. No onchain action is required to create and close a virtual channel.

A virtual channel has characteristics such as: 
* Key
* Lifetime 
* Capacity
* Commission

For example, `A`, `B` and `C` have open channels on the blockchain (`A->B`, `B->C`), and there is no onchain channel `A->C`, but `A` can create a virtual channel to `C` by asking `B` to proxy through his channel for a small fee. Thus the chain would be `A->B->C`.

Looking at the example above, the question arises - what if `B` takes coins from `A` and does not pass them to `C`? 
The answer is that `B` will not be able to do so thanks to elliptic cryptography and the flexible TON blockchain.

When `A` asks `B` to open a virtual channel, he does not transfer the money immediately, but only gives `B` a signed guarantee that if `C` provides confirmation of receipt of the transfer from `A`, `B` will transfer the requested amount to `C`. Then `B` can request the same amount + commission from `A`, on the same confirmation it received from `C`. And so on down the chain if its length is greater than 3.

Each link in the chain opens virtual channels with each other, starting from the initiator and ending at the destination point. The conditions may vary depending on the agreements between nodes, but the key always remains the same, which allows closing the channel to all participants by passing an acknowledgement down the chain. The conditions cascade from sender to receiver and include each other. For example, if in a chain of 4 links 2 take a 0.01 TON commission, the sender will send 0.02 TON commission, half of which the next link will pass on. The lifetime of a link is always decreased from sender to receiver to prevent node cheating, which is to close the link at the last moment so that the node does not have time to close its part with the next neighbor.

In case one of the nodes along the path does not agree to open the channel with the next node, the channel will be rolled back down the chain and the channel capacity will be unlocked for the sender. In the worst case, it may happen that one of the nodes will act out of order and will not agree to rollback or will not respond to the channel opening. In this case, the capacity will be unlocked after the lifetime specified in the channel.

#### Security guarantees

The whole process takes place without interacting with the blockchain, hence no network commission is paid. You only have to interact with the blockchain in case of disagreements, for example, if a neighbor in the chain does not behave according to the rules and refuses to hand over coins in exchange for proof. Then you can simply send this proof to the contract, closing it, and get your money - everything is insured.

Virtual channels are implemented using conditional payments, the conditions of which are described by the following logic:
```c
int cond(slice input, int fee, int capacity, int deadline, int key) {
slice sign = input~load_bits(512);
throw_unless(24, check_data_signature(input, sign, key));
throw_unless(25, deadline >= now());

int amount = input~load_coins();
throw_unless(26, amount <= capacity);

return amount + fee;
}
```

The logic of conditional payments is performed offchein if the parties agree, and onchain if they disagree.

#### Anonymity of the virtual channel

All links of the chain are known only to the creator of the virtual channel, as he forms the chain. The other links in the chain know only those who have opened a channel with them and those with whom they need to open a virtual channel. It is not possible to directly identify whether the sender or receiver is the end link or an intermediate link.

This is achieved by the sender forming the chain in 'Garlic' form, where the jobs are packaged and encrypted with a shared key and can only be decrypted by the person to whom the job is intended. In addition to real jobs, non-existent jobs are passed in for bulk.

A task consists of a description of what the node should receive from the previous neighbor and what to pass to the next neighbor. Neighbors cannot cheat each other, since the expected values are described in the assignment, and the channel will simply diverge if violated.

#### Networking interactions in a chain

Networking is built on two basic actions, `ProposeAction` and `RequestAction`.

* `Propose` - involves transmitting a signed modified channel state describing the desired change, e.g., open a virtual channel. The neighbor can either accept or refuse. In case of refusal, it must acknowledge its refusal with a signature. Each `Propose` action is transactional and must be executed in full or rolled back on both sides. In case of network errors, the action is repeated until it is either accepted or rejected with a signature. All actions are executed strictly sequentially within the channel. 

* `Request` - requests a neighboring node to do `Propose`, e.g. close a virtual channel.

### Speed, reliability and cross-platform.

Opening a virtual channel, while complicated, is very fast. It takes about 3 milliseconds to process an action on the node side on a normal working computer. This means that the server can open and close > 300 virtual channels per second without much effort. This figure can be greatly increased in the future, with improved lock separation.

All important actions are performed using a special queue, which is written to transactionally with other actions, and acknowledge commits to disk (ACID). The current database implementation is built on top of the built-in LevelDB. 

The implementation is in pure Golang, and the code can be compiled for all platforms, including mobile.

#### Node management

At startup the `-name {seed}` flag is specified, where `{seed}` is any word from which a private key and wallet will be generated, the wallet address will be displayed in the console, and it needs to be replenished with test coins before further actions.

At the moment payment node as a standalone service supports several console commands:

* `list` - Display a list of active onchain and virtual channels.
* `deploy` - Deploy a channel with a node that has a key (trace command) and balance.
* `open` - Open a virtual channel with the further entered key, using the further entered onchain channel as a tunnel. Generates and returns a private key for the virtual channel.
* `send` - Send coins using self-closing virtual channel after chain initialization. Parameters are similar to `open`.
* `sign` - Accepts the virtual channel private key and amount as input, returns a steit in hex format that the other side can use to close the virtual channel.
* `close` - Closes the virtual channel, asks for a sign steit as input. Closing should be done by the recipient.
* `destroy` - Close the onchain channel with the address specified below, at first we try cooperatively, if it fails, on our own.

There is also a deployed node to cooperate with, its public key is `fdf66ea12228f2dab720d3f4deffc82d8a10eef7400ff604aa5d4e7e80758370`.

### HTTP API

Node can be controlled programmatically through the API, below is a description of the supported methods

#### GET /api/v1/channel/onchain

Query parameter `address` must be set to channel address.

Response example:
```json
{
  "id": "3e4c462d14277d25e89b063e4df4e000",
  "address": "EQAxZGOOZAXU5XhCAp8bbGG5xQZfGhc6ppHrdIXJrla6Ji8i",
  "accepting_actions": true,
  "status": "active",
  "we_left": true,
  "our": {
    "key": "fdf66ea12228f2dab720d3f4deffc82d8a10eef7400ff604aa5d4e7e80758370",
    "available_balance": "0",
    "onchain": {
      "committed_seqno": 0,
      "wallet_address": "EQARsvGCV5t-iXkOA97DwksSv_nKC5obhYnysnc3V4YZW8el",
      "deposited": "0.1"
    }
  },
  "their": {
    "key": "3e4c462d14277d25e89b063e4df4e0476d4f5729c11da0ea716d7003cc6ba26c",
    "available_balance": "0",
    "onchain": {
      "committed_seqno": 0,
      "wallet_address": "EQCVgVWnMWAXsjrWci0kUTUVaHI7Lxa7lqMIHyGSTTOxqXUm",
      "deposited": "0"
    }
  },
  "init_at": "2024-02-04T14:39:00Z",
  "updated_at": "2024-02-04T14:39:00Z",
  "created_at": "2024-02-04T14:39:10.094014354Z"
}
```

#### GET /api/v1/channel/onchain/list

Returns all onchain channels, supports filtering with query parameters `status` (active | closing | inactive | any) and `key` (hex neighbour node key)

Response example:
```json
[
  {
    "id": "3e4c462d14277d25e89b063e4df4e000",
    "address": "EQAxZGOOZAXU5XhCAp8bbGG5xQZfGhc6ppHrdIXJrla6Ji8i",
    "accepting_actions": true,
    "status": "active",
    "we_left": true,
    "our": {
      "key": "fdf66ea12228f2dab720d3f4deffc82d8a10eef7400ff604aa5d4e7e80758370",
      "available_balance": "0",
      "onchain": {
        "committed_seqno": 0,
        "wallet_address": "EQARsvGCV5t-iXkOA97DwksSv_nKC5obhYnysnc3V4YZW8el",
        "deposited": "0.1"
      }
    },
    "their": {
      "key": "3e4c462d14277d25e89b063e4df4e0476d4f5729c11da0ea716d7003cc6ba26c",
      "available_balance": "0",
      "onchain": {
        "committed_seqno": 0,
        "wallet_address": "EQCVgVWnMWAXsjrWci0kUTUVaHI7Lxa7lqMIHyGSTTOxqXUm",
        "deposited": "0"
      }
    },
    "init_at": "2024-02-04T14:39:00Z",
    "updated_at": "2024-02-04T14:39:00Z",
    "created_at": "2024-02-04T14:39:10.094014354Z"
  },
  {
    "id": "fdf66ea12228f2dab720d3f4deffc800",
    "address": "EQCEFA5lzhJbJGIWoSokRoJFeEMisCON-qlvVUgZjwyGDoxR",
    "accepting_actions": true,
    "status": "active",
    "we_left": false,
    "our": {
      "key": "fdf66ea12228f2dab720d3f4deffc82d8a10eef7400ff604aa5d4e7e80758370",
      "available_balance": "0.1",
      "onchain": {
        "committed_seqno": 0,
        "wallet_address": "EQARsvGCV5t-iXkOA97DwksSv_nKC5obhYnysnc3V4YZW8el",
        "deposited": "0"
      }
    },
    "their": {
      "key": "3e4c462d14277d25e89b063e4df4e0476d4f5729c11da0ea716d7003cc6ba26c",
      "available_balance": "0.1",
      "onchain": {
        "committed_seqno": 0,
        "wallet_address": "EQCVgVWnMWAXsjrWci0kUTUVaHI7Lxa7lqMIHyGSTTOxqXUm",
        "deposited": "0.2"
      }
    },
    "init_at": "2024-02-04T14:23:59Z",
    "updated_at": "2024-02-06T12:38:20Z",
    "created_at": "2024-02-04T14:24:09.3526702Z"
  }
]
```

#### POST /api/v1/channel/onchain/open

Connects to neighbour node by its key and deploys onchain channel contract with it.

Requires body parameters: `with_node` - hex neighbour node key, `capacity` - amount of ton to add to initial balance.

Optional body parameters: `jetton_master` - jetton master address, if not specified payment channel will use ton.

Request:
```json
{
  "with_node": "3e4c462d14277d25e89b063e4df4e0476d4f5729c11da0ea716d7003cc6ba26c",
  "capacity": "5.52",
  "jetton_master": "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
}
```

Response example:
```json
{
  "address": "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N"
}
```

`address` - is onchain channel contract address

#### POST /api/v1/channel/onchain/close

Closes onchain channel with neighbour node.

Requires body parameters: `address` - channel contract address.

Optional parameters: `force` - boolean, indicates a style of channel closure, if `true`, do it uncooperatively (onchain).
If false or not specified, tries to do it cooperatively first.

Request:
```json
{
  "address": "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N",
  "force": false
}
```

Response example:
```json
{
  "success": true
}
```

#### POST /api/v1/channel/virtual/open

Opens virtual channel using specified chain and parameters.

Requires body parameters: `ttl_seconds` - virtual channel life duration, `capacity` - max transferable amount. `nodes_chain` - list of nodes with parameters to build chain.

Node parameters: `deadline_gap_seconds` - seconds to increase channel lifetime for safety reasons, can be got from node parameters, same as `fee` which will be paid to proxy node for the service after channel close. `key` - node key.

Last node is considered as final destination.

Request:
```json
{
  "ttl_seconds": 86400,
  "capacity": "3.711",
  "nodes_chain": [
    {
      "key": "3e4c462d14277d25e89b063e4df4e0476d4f5729c11da0ea716d7003cc6ba26c",
      "fee": "0.005",
      "deadline_gap_seconds": 1800
    },
    {
      "key": "1e4c462d14277d25e89b063e4df4e0472d4f5729c11da0ea716d7003cc6ba11f",
      "fee": "0",
      "deadline_gap_seconds": 1800
    }
  ]
}
```

Response example:
```json
{
  "public_key": "af9ad86e9201d7c2b930f6a2707475bfa84faf3633729ec7139c0592d2823d6b",
  "private_key_seed": "095822d7dc66312d59dd54311d665f26748229bd3a67c80391baef6745e39cf8",
  "status": "pending",
  "deadline": "2024-02-07T07:55:43+00:00"
}
```

#### POST /api/v1/channel/virtual/transfer

Transfer by auto-closing virtual channel using specified chain and parameters.

Requires body parameters: `ttl_seconds` - virtual channel life duration, `amount` - transfer amount. `nodes_chain` - list of nodes with parameters to build chain.

Node parameters: `deadline_gap_seconds` - seconds to increase channel lifetime for safety reasons, can be got from node parameters, same as `fee` which will be paid to proxy node for the service after channel close. `key` - node key.

Last node is considered as final destination.

Request:
```json
{
  "ttl_seconds": 3600,
  "amount": "2.05",
  "nodes_chain": [
    {
      "key": "3e4c462d14277d25e89b063e4df4e0476d4f5729c11da0ea716d7003cc6ba26c",
      "fee": "0.005",
      "deadline_gap_seconds": 300
    },
    {
      "key": "1e4c462d14277d25e89b063e4df4e0472d4f5729c11da0ea716d7003cc6ba11f",
      "fee": "0",
      "deadline_gap_seconds": 300
    }
  ]
}
```

Response example:
```json
{
  "status": "pending",
  "deadline": "2024-02-07T07:55:43+00:00"
}
```

#### POST /api/v1/channel/virtual/close

Close virtual channel using specified state.

Requires body parameters: `key` - virtual channel public key, `state` - signed hex state to close channel with.

Request:
```json
{
  "key": "af9ad86e9201d7c2b930f6a2707475bfa84faf3633729ec7139c0592d2823d6b",
  "state": "f509a550365e4fbb75479b076cf6144d52b20fd97d21d9f9d3873df3fe9615918628129551a29480498744c3b412e590446a632db92204d0e48dadc177624ae2cb123cd6659eceaec432f77d6b2820ca1b6e7006b95163c9942e680b9afed0650bdb2f5513f9219eaad4809209106f02ccff31eb66be9ee8b0c03f78a90dee90623ceb9e2eda39e916ecbb8015771d0d13f615c6d279f26e1f3af56544f283e3",
}
```

Response example:
```json
{
  "success": true
}
```

#### POST /api/v1/channel/virtual/state

Save virtual channel state to not lose it.

Requires body parameters: `key` - virtual channel public key, `state` - signed hex state to save.

Request:
```json
{
  "key": "af9ad86e9201d7c2b930f6a2707475bfa84faf3633729ec7139c0592d2823d6b",
  "state": "f509a550365e4fbb75479b076cf6144d52b20fd97d21d9f9d3873df3fe9615918628129551a29480498744c3b412e590446a632db92204d0e48dadc177624ae2cb123cd6659eceaec432f77d6b2820ca1b6e7006b95163c9942e680b9afed0650bdb2f5513f9219eaad4809209106f02ccff31eb66be9ee8b0c03f78a90dee90623ceb9e2eda39e916ecbb8015771d0d13f615c6d279f26e1f3af56544f283e3",
}
```

Response example:
```json
{
  "success": true
}
```

#### GET /api/v1/channel/virtual/list

Returns all virtual channels of onchain channel specified with `address` query parameter.

Response example:
```json
{
  "their": [
    {
      "key": "1e8bd2e8a72fd005d9c7b1b144d5d2634906c681dacee4475ef9798118142b30",
      "status": "active",
      "amount": "0",
      "outgoing": null,
      "incoming": {
        "channel_address": "EQC0K4-WwDACT8XxWO4A5zYMi5W9np9CdbPd34OxO33Bq73L",
        "capacity": "0.2",
        "fee": "0",
        "deadline_at": "2024-02-07T13:35:49Z"
      },
      "created_at": "2024-02-07T12:06:11.177563296Z",
      "updated_at": "2024-02-07T12:06:11.177563426Z"
    }
  ],
  "our": null
}
```

#### GET /api/v1/channel/virtual

Returns virtual channel specified with `key` (virtual channel's public key) query parameter.

Response example:
```json
{
  "key": "1e8bd2e8a72fd005d9c7b1b144d5d2634906c681dacee4475ef9798118142b30",
  "status": "active",
  "amount": "0",
  "outgoing": null,
  "incoming": {
    "channel_address": "EQC0K4-WwDACT8XxWO4A5zYMi5W9np9CdbPd34OxO33Bq73L",
    "capacity": "0.2",
    "fee": "0",
    "deadline_at": "2024-02-07T13:35:49Z"
  },
  "created_at": "2024-02-07T12:06:11.177563296Z",
  "updated_at": "2024-02-07T12:06:11.177563426Z"
}
```


### Roadmap

* Opening a virtual channel with someone without a wallet on the network (for transferring coins to them before the contract is deposited)
* Virtual channels in the form of MerkleProof to support virtually unlimited number of active virtual channels per onchain channel.
* Status updates via MerkleUpdate.
* Support for Postgres as an alternative data store.
* API and Webhook events
