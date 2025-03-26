<table>
  <tr>
    <td width="130">
      <img src="https://github.com/user-attachments/assets/210ef0fe-8a50-4635-b068-090bdf03006c" width="125px">
    </td>
    <td>
      <h1>TON Payment Network</h1>
      <p>This is an implementation of a <strong>peer-to-peer payment network</strong> with <strong>multi-node routing</strong>, powered by the <strong>TON Blockchain</strong>.<br>
      More powerful than Lightning!</p>
    </td>
  </tr>
</table>

## Technical Description

The network consists of **peer-to-peer payment nodes** that establish connections between each other via:

- **Smart contract deployment** to the blockchain
- **RLDP protocol** for network communication

A payment node can be:

- A **standalone service** — if the main goal is to **earn fees** from serving virtual channel chains
- A **library integrated into other apps** — if the goal is to **pay or get paid for services**, like **TON Storage** or **TON Proxy**

---

### Example Interactions

![Diagram](https://github.com/xssnick/ton-payment-network/assets/9332353/c127d64f-2f04-4e70-87e6-252d08d1ce47)

---

### Onchain Channels

The onchain channel contract supports:

- **TON coins**
- **Jettons**
- **ExtraCurrencies**

#### Channel Discovery

Discovery works by:

- **Scanning new blocks** in the network to catch contract updates
- Or **listening to contract events** (for lite embedded nodes)

For example:

- Detecting deployment of new contracts with known keys when someone wants to establish a channel
- Catching events related to **uncoordinated closures**

#### Establishing a Channel

If a node wants to connect with another node:

1. It connects to other node to check if it exists, and reach agreement on future channel configuration. (e.g. closure timeframe, misbehaviour fines)
2. It **deploys a payment channel contract** to the blockchain.
3. The contract includes **two public keys**: the node's own key and the target node’s key.
4. The target node detects the contract, verifies its parameters, and if everything is correct — **allows channel specific communication** after authentication.

#### Authentication

For network authentication:

- **Channel keys** are used.
- A special **authentication message** is created from the **ADNL addresses** of both parties and a **timestamp**, signed with the **channel key**.
- The response must include:
    - The **reversed ADNL addresses**
    - The **same timestamp**
    - A **valid signature** from the other party

### Virtual Channels

A virtual channel can be opened between **any two nodes** in the network, as long as there is a **chain of links** between them — including an onchain contract and an active network connection.  
**No onchain action** is required to create or close a virtual channel.

---

A virtual channel has the following characteristics:

- **Key**
- **Lifetime**
- **Capacity**
- **Fee**

---

For example:

If nodes `A`, `B`, and `C` have the following onchain channels:

- `A → B`
- `B → C`

There is **no direct onchain channel `A → C`**, but `A` can still open a **virtual channel** to `C` by asking `B` to **proxy** the connection — for a small fee.  
Thus, the chain becomes: `A → B → C`.

---

#### Security: Why can't `B` steal `A`'s funds?

Thanks to **elliptic cryptography** and the **flexible nature of the TON blockchain**, `B` cannot simply take the funds from `A` and avoid forwarding them to `C`.

Here’s how it works:

- When `A` requests `B` to open a virtual channel, `A` does **not immediately send funds**, but gives `B` a **signed guarantee**.
- This guarantee states: if `C` provides a **confirmation of receipt** from `A`, then `B` will transfer the funds to `C`.
- Then `B` can request the same amount **plus a fee** from `A`, using the same **confirmation received from `C`**.
- This pattern repeats along the chain if it involves more than 3 nodes.

---

Each link in the chain creates a virtual channel with its neighbor, starting from the **initiator** and ending at the **final recipient**.

- **Conditions** may vary per node agreement, but the **key is shared**, allowing for **channel closure** by passing an acknowledgement down the chain.
- **Fees cascade**:  
  If there are 4 links and 2 of them charge a **0.01 TON** fee, the sender will send a total **0.02 TON** in fees — each node forwarding part of it to the next.
- **Lifetime cascades**:  
  Lifetime decreases from sender to receiver, preventing cheating (e.g., one node closing at the last moment to block others from acting in time).

---

##### Rollbacks and Fallbacks

- If **any node** in the chain refuses to open the channel with the next node, the **entire channel is rolled back** and capacity is unlocked for the sender.
- In rare worst-case scenarios (e.g., a node misbehaves or fails to respond), the capacity will be automatically **unlocked after the channel’s lifetime expires**.

#### Safety Guarantees

The entire process works **offchain**, meaning there is **no interaction with the blockchain** during normal operation — and therefore, **no network fees** are paid.

You only need to interact with the blockchain in **case of a dispute** — for example, if a node in the chain **violates the rules** and refuses to transfer funds after receiving valid proof.

In such a case, you can simply **submit the proof to the smart contract**, which will **automatically close the channel** and return your funds.  
Everything is **insured by design**.

---

**Virtual channels** are implemented using **conditional payments**, governed by the following logic:

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

- The logic of conditional payments is executed **offchain** when both parties agree.
- If there is a **disagreement**, the same logic is executed **onchain** by the smart contract.


##### Virtual Channels and Onchain Settlement

- **The number of virtual channels per one onchain channel** is theoretically unlimited.  
  This is achieved by using Merkle proofs to commit conditionals onchain.

- **The channel stores only the Merkle tree root hash** and updates it after each transaction during the settlement phase.

- **Settlement can be performed through any number of transactions.**  
  Each transaction carries a Merkle proof that includes only the conditions to be resolved in that transaction.

- **After a conditional is resolved**, its corresponding value in the dictionary tree is replaced with an empty cell,  
  and the Merkle root hash is updated.  
  This ensures that no conditional can be resolved more than once.

#### Privacy of the Virtual Channel

The full structure of the virtual channel chain is known **only to the creator** of the channel, as they are the one who forms the chain.  
Other participants are aware of:

- The node that **opened a channel with them**
- The node they must **open a virtual channel with**

It is **not possible to tell** whether a node is the **sender**, **receiver**, or an **intermediary**.

---

This privacy is achieved through a **"Garlic"-style encrypted task chain**, where:

- Each task is **individually encrypted** with a shared key for the specific recipient
- Only the intended node can **decrypt and read** its task
- **Dummy (fake) tasks** are added to the chain to obscure the real routing structure

---

Each **task** includes:

- Instructions on what the node should **expect from the previous node**
- What it should **pass to the next node**

Nodes **cannot cheat** each other, as each step is verifiable through the task data.  
If a mismatch occurs, the **channel fails** and is **rolled back** safely.

#### Networking Interactions in a Chain

Networking is based on two core actions: `ProposeAction` and `RequestAction`.

- **`Propose`**  
  This action involves transmitting a **signed, modified channel state** that describes the intended change — for example, opening a virtual channel.  
  The neighboring node can either **accept** or **refuse** the proposal.

    - Even in case of **refusal**, the neighbor must acknowledge it with a **signature**.
    - Each `Propose` action is **transactional** — it must be either **fully completed** or **rolled back** on both sides.
    - If there is a **network error**, the action is retried until it is either **accepted** or **rejected with a signature**.
    - All `Propose` actions are executed **strictly sequentially** within the channel.

- **`Request`**  
  This action is used to ask the neighboring node to perform a `Propose` — for example, to **close a virtual channel**.

##### Distributed Lock for Channel State Changes

For actions that modify the channel state, a **distributed lock** mechanism is used.

- The `left` node is considered the **lock master**.
- When the `right` node wants to change the channel state, it must **request the `left` node** to lock the state and **wait for the changes**.
- The `left` (master) node can lock the channel **by itself**, as long as it is not already locked by the `right` node.

This mechanism is necessary to **prevent concurrent modifications** and **avoid potential rollback issues**.

It is designed for safety, but can be **optimized later** if performance becomes a concern.

### Speed, Reliability, and Cross-Platform

Opening a virtual channel, while technically complex, is **very fast** — it takes only **milliseconds** to process an action on the node side, even on basic hardware.  
Performance can be further improved in the future with **enhanced lock separation**.

All critical operations are executed through a **special action queue**, which is processed **transactionally** (ACID-compliant) and commits to disk reliably.  
The current database implementation is based on **LevelDB**.

The entire implementation is written in **pure Golang**, making it **cross-platform** — including support for **mobile platforms**.

---

#### Standalone Node Management and First Startup

On the **first startup**, a `payment-network-config.json` config file and `payment-node-db` folder are generated.

This wallet address will appear in the logs — it must be **funded** with TON and desired coins before any operations can proceed.

Wallet and node keys, EC and Jetton Master addresses whitelist can be changed in config, if needed.

---

The standalone node currently supports several **console commands**:

- `list` — Display all active **onchain** and **virtual** channels.
- `deploy` — Deploy a channel with another node using its key.
- `topup` — Add funds to a channel.
- `open` — Open a **virtual channel** using:
    - A key for the counterparty
    - An onchain channel as a tunnel  
      This command also returns a **private key** for the virtual channel.
- `send` — Send coins using a **self-closing virtual channel**.  
  Parameters are similar to `open`.
- `sign` — Accepts a virtual channel private key and amount, returns a **hex-encoded state** that the counterparty can use to close the virtual channel.
- `close` — Close a virtual channel (used by the **recipient**) by providing a signed state.
- `destroy` — Close an **onchain channel** by address.  
  First attempts a **cooperative closure**, and if that fails, performs a **forced closure**.

---

### HTTP API

The node can be **controlled programmatically** through an HTTP API.

---

#### Webhooks

You can subscribe to **webhook events** to receive updates about:

- **Onchain balances**
- **Channel state changes**

To subscribe, run the node with the `-webhook` argument:

```bash
-webhook http://127.0.0.1:8080/some/url
```

This sets your backend API as the destination for webhook events.

- Requests are signed using **HMAC SHA256**
- The signing key is available in your node’s `config.json`
- Signatures are sent in the `Signature` header, encoded in **base64**

If your backend responds with a status code other than `200`, the **webhook will be retried**.

Basic webhook body structure:
```json
{
  "id": "abc-123-112...",
  "type": "onchain-channel-event",
  "data": { ... },
  "event_time": "2024-02-04T14:39:00Z"
}
```

##### Onchain event structure (type = `onchain-channel-event`)
```go
type Onchain struct {
	CommittedSeqno uint32 `json:"committed_seqno"`
	WalletAddress  string `json:"wallet_address"`
	Deposited      string `json:"deposited"`
	Withdrawn      string `json:"withdrawn"`
}

type Side struct {
	Key              string  `json:"key"`
	AvailableBalance string  `json:"available_balance"`
	Onchain          Onchain `json:"onchain"`
}

type OnchainChannel struct {
	ID               string `json:"id"`
	Address          string `json:"address"`
	JettonAddress    string `json:"jetton_address"`
	ExtraCurrencyID  uint32 `json:"ec_id"`
	AcceptingActions bool   `json:"accepting_actions"`
	Status           string `json:"status"`
	WeLeft           bool   `json:"we_left"`
	Our              Side   `json:"our"`
	Their            Side   `json:"their"`

	InitAt          time.Time `json:"init_at"`
	CreatedAt       time.Time `json:"created_at"`
	LastProcessedLT uint64    `json:"processed_lt"`
}
```
`OnchainChannel` will be sent in `data` field.

##### Virtual event structure (type = `virtual-channel-event`)
```go
type VirtualSide struct {
	ChannelAddress string    `json:"channel_address"`
	Capacity       string    `json:"capacity"`
	Fee            string    `json:"fee"`
	DeadlineAt     time.Time `json:"deadline_at"`
}

type VirtualChannel struct {
	Key       string       `json:"key"`
	Status    string       `json:"status"`
	Amount    string       `json:"amount"`
	Outgoing  *VirtualSide `json:"outgoing"`
	Incoming  *VirtualSide `json:"incoming"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}
```
`VirtualChannel` will be sent in `data` field.


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

Optional body parameters: `ec_id` - extra currency id, `jetton_master` - jetton master address, (only one of them or none should be set) if not specified payment channel will use ton.

Request:
```json
{
   "with_node": "3e4c462d14277d25e89b063e4df4e0476d4f5729c11da0ea716d7003cc6ba26c",
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

#### POST /api/v1/channel/onchain/topup

Creates task to topup onchain channel balance of our side. 
You can call topup multiple times while channel is active.
Coins will be taken from node's wallet.

Requires body parameters: `address` - contract address, `amount_nano` - amount of coin to add to balance.

Request:
```json
{
   "address": "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N",
   "amount_nano": 1000000000
}
```

Response example:
```json
{
   "success": true
}
```

#### POST /api/v1/channel/onchain/withdraw

Creates task to withdraw from onchain channel.
You can call withdraw multiple times while channel is active.
Coins will be sent to node's wallet.

Withdraw happens only after confirmation by other party. 
Transaction requires some gas fee, so don't call it for very little amounts.

Requires body parameters: `address` - contract address, `amount_nano` - amount of coin to withdraw from balance.

Request:
```json
{
   "address": "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N",
   "amount_nano": 1000000000
}
```

Response example:
```json
{
   "success": true
}
```

#### POST /api/v1/channel/onchain/close

Closes onchain channel with neighbour node.

Requires body parameters: `address` - channel contract address.

Optional parameters: `force` - boolean, indicates a style of channel closure, if `true`, do it uncooperative (onchain).
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
   "state": "f509a550365e4fbb75479b076cf6144d52b20fd97d21d9f9d3873df3fe9615918628129551a29480498744c3b412e590446a632db92204d0e48dadc177624ae2cb123cd6659eceaec432f77d6b2820ca1b6e7006b95163c9942e680b9afed0650bdb2f5513f9219eaad4809209106f02ccff31eb66be9ee8b0c03f78a90dee90623ceb9e2eda39e916ecbb8015771d0d13f615c6d279f26e1f3af56544f283e3"
}
```

Response example:
```json
{
   "success": true
}
```

#### POST /api/v1/channel/virtual/state

Save virtual channel state. Call it each time when you receive update, to have actual state on closure.

Requires body parameters: `key` - virtual channel public key, `state` - signed hex state to save.

Request:
```json
{
   "key": "af9ad86e9201d7c2b930f6a2707475bfa84faf3633729ec7139c0592d2823d6b",
   "state": "f509a550365e4fbb75479b076cf6144d52b20fd97d21d9f9d3873df3fe9615918628129551a29480498744c3b412e590446a632db92204d0e48dadc177624ae2cb123cd6659eceaec432f77d6b2820ca1b6e7006b95163c9942e680b9afed0650bdb2f5513f9219eaad4809209106f02ccff31eb66be9ee8b0c03f78a90dee90623ceb9e2eda39e916ecbb8015771d0d13f615c6d279f26e1f3af56544f283e3"
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

### Integration Scenario for Wallets

As a wallet, you can operate your own payment node to serve your users.  
Set it up as a standalone node and inject its ID into the wallet application.

Additionally, build the payment node as a native library and integrate it into your mobile or desktop app.

When a user wants to join the payment network, they deploy a contract using your payment node's key.  
(This can be done using methods from the payment library.)

---

- If the user only wants to **pay for services**, they simply need to top up the contract from their side.

- If the user wants to **send and receive payments**, both you and the user need to deposit some amount into the contract.  
  (The user can rent your locked coins — for example, by paying a monthly fee to keep an amount reserved.)

---

Once the contract is funded, the user can immediately start making **offchain payments** and open **virtual channels** with any linked services or other users.

The user can **top up their channel at any time**, and **withdraw coins back to their wallet** when needed.

To request actions from your node (e.g., depositing to the user's wallet),  
you can use the **HTTP API** and **webhooks** as described above.
