# HTTP API

The node can be **controlled programmatically** through an HTTP API.

---

#### GET /api/v1/channel/onchain

Query parameter `address` must be set to channel address.

Response example:
```json
{
   "id": "PkxGLRQnfSXomwY+TfTgAA==",
   "address": "EQAxZGOOZAXU5XhCAp8bbGG5xQZfGhc6ppHrdIXJrla6Ji8i",
   "accepting_actions": true,
   "status": "active",
   "we_left": true,
   "our": {
      "key": "/fZuoSIo8tq3INP03v/ILYoQ7vdAD/YEql1OfoB1g3A=",
      "available_balance": "0",
      "onchain": {
         "committed_seqno": 0,
         "wallet_address": "EQARsvGCV5t-iXkOA97DwksSv_nKC5obhYnysnc3V4YZW8el",
         "deposited": "0.1"
      }
   },
   "their": {
      "key": "PkxGLRQnfSXomwY+TfTgR21PVynBHaDqcW1wA8xromw=",
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
      "id": "PkxGLRQnfSXomwY+TfTgAA==",
      "address": "EQAxZGOOZAXU5XhCAp8bbGG5xQZfGhc6ppHrdIXJrla6Ji8i",
      "accepting_actions": true,
      "status": "active",
      "we_left": true,
      "our": {
         "key": "/fZuoSIo8tq3INP03v/ILYoQ7vdAD/YEql1OfoB1g3A=",
         "available_balance": "0",
         "onchain": {
            "committed_seqno": 0,
            "wallet_address": "EQARsvGCV5t-iXkOA97DwksSv_nKC5obhYnysnc3V4YZW8el",
            "deposited": "0.1"
         }
      },
      "their": {
         "key": "PkxGLRQnfSXomwY+TfTgR21PVynBHaDqcW1wA8xromw=",
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
      "id": "/fZuoSIo8tq3INP03v/IAA==",
      "address": "EQCEFA5lzhJbJGIWoSokRoJFeEMisCON-qlvVUgZjwyGDoxR",
      "accepting_actions": true,
      "status": "active",
      "we_left": false,
      "our": {
         "key": "/fZuoSIo8tq3INP03v/ILYoQ7vdAD/YEql1OfoB1g3A=",
         "available_balance": "0.1",
         "onchain": {
            "committed_seqno": 0,
            "wallet_address": "EQARsvGCV5t-iXkOA97DwksSv_nKC5obhYnysnc3V4YZW8el",
            "deposited": "0"
         }
      },
      "their": {
         "key": "PkxGLRQnfSXomwY+TfTgR21PVynBHaDqcW1wA8xromw=",
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

Requires body parameters: `with_node` - base64 neighbour node key, `capacity` - amount of ton to add to initial balance.

Optional body parameters: `ec_id` - extra currency id, `jetton_master` - jetton master address, (only one of them or none should be set) if not specified payment channel will use ton.

Request:
```json
{
   "with_node": "PkxGLRQnfSXomwY+TfTgR21PVynBHaDqcW1wA8xromw=",
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

Optional body parameters: `ec_id` - extra currency id, `jetton_master` - jetton master address, (only one of them or none should be set) if not specified payment channel will use ton.

Node parameters: `deadline_gap_seconds` - seconds to increase channel lifetime for safety reasons, can be got from node parameters, same as `fee` which will be paid to proxy node for the service after channel close. `key` - node key.

Last node is considered as final destination.

Request:
```json
{
   "ttl_seconds": 86400,
   "capacity": "3.711",
   "jetton_master": "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs",
   "nodes_chain": [
      {
         "key": "PkxGLRQnfSXomwY+TfTgR21PVynBHaDqcW1wA8xromw=",
         "fee": "0.005",
         "deadline_gap_seconds": 1800
      },
      {
         "key": "HkxGLRQnfSXomwY+TfTgRy1PVynBHaDqcW1wA8xroR8=",
         "fee": "0",
         "deadline_gap_seconds": 1800
      }
   ]
}
```

Response example:
```json
{
   "public_key": "r5rYbpIB18K5MPaicHR1v6hPrzYzcp7HE5wFktKCPWs=",
   "private_key_seed": "CVgi19xmMS1Z3VQxHWZfJnSCKb06Z8gDkbrvZ0XjnPg=",
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
         "key": "PkxGLRQnfSXomwY+TfTgR21PVynBHaDqcW1wA8xromw=",
         "fee": "0.005",
         "deadline_gap_seconds": 300
      },
      {
         "key": "HkxGLRQnfSXomwY+TfTgRy1PVynBHaDqcW1wA8xroR8=",
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

Requires body parameters: `key` - virtual channel public key, `state` - signed base64 state to close channel with.

Request:
```json
{
   "key": "r5rYbpIB18K5MPaicHR1v6hPrzYzcp7HE5wFktKCPWs=",
   "state": "9QmlUDZeT7t1R5sHbPYUTVKyD9l9Idn504c98/6WFZGGKBKVUaKUgEmHRMO0EuWQRGpjLbkiBNDkja3Bd2JK4ssSPNZlns6uxDL3fWsoIMobbnAGuVFjyZQuaAua/tBlC9svVRP5IZ6q1ICSCRBvAsz/Metmvp7osMA/eKkN7pBiPOueLto56Rbsu4AVdx0NE/YVxtJ58m4fOvVlRPKD4w=="
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

Requires body parameters: `key` - virtual channel public key, `state` - signed base64 state to save.

Request:
```json
{
   "key": "r5rYbpIB18K5MPaicHR1v6hPrzYzcp7HE5wFktKCPWs=",
   "state": "9QmlUDZeT7t1R5sHbPYUTVKyD9l9Idn504c98/6WFZGGKBKVUaKUgEmHRMO0EuWQRGpjLbkiBNDkja3Bd2JK4ssSPNZlns6uxDL3fWsoIMobbnAGuVFjyZQuaAua/tBlC9svVRP5IZ6q1ICSCRBvAsz/Metmvp7osMA/eKkN7pBiPOueLto56Rbsu4AVdx0NE/YVxtJ58m4fOvVlRPKD4w=="
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
         "key": "HovS6Kcv0AXZx7GxRNXSY0kGxoHazuRHXvl5gRgUKzA=",
         "status": "active",
         "amount": "0",
         "outgoing": null,
         "incoming": {
            "channel_address": "EQC0K4-WwDACT8XxWO4A5zYMi5W9np9CdbPd34OxO33Bq73L",
            "capacity": "0.2",
            "fee": "0",
            "uncooperative_deadline_at": "2024-02-07T13:35:49Z",
            "safe_deadline_at": "2024-02-07T13:35:49Z"
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
   "key": "HovS6Kcv0AXZx7GxRNXSY0kGxoHazuRHXvl5gRgUKzA=",
   "status": "active",
   "amount": "0",
   "outgoing": null,
   "incoming": {
      "channel_address": "EQC0K4-WwDACT8XxWO4A5zYMi5W9np9CdbPd34OxO33Bq73L",
      "capacity": "0.2",
      "fee": "0",
      "uncooperative_deadline_at": "2024-02-07T13:35:49Z",
      "safe_deadline_at": "2024-02-07T13:35:49Z"
   },
   "created_at": "2024-02-07T12:06:11.177563296Z",
   "updated_at": "2024-02-07T12:06:11.177563426Z"
}
```

---

## Webhooks

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
    ChannelAddress          string    `json:"channel_address"`
    Capacity                string    `json:"capacity"`
    Fee                     string    `json:"fee"`
    UncooperativeDeadlineAt time.Time `json:"uncooperative_deadline_at"`
    SafeDeadlineAt          time.Time `json:"safe_deadline_at"`
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
