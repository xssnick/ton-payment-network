# TON Payment Network

<img align="right" src="https://github.com/user-attachments/assets/ff51e55e-2cfe-4fcf-9100-3d62c6fc5a1b" width="420px">

TON Payment Network is a fast, scalable, and low-cost **peer-to-peer payment network** built on top of the TON Blockchain.  
It allows users to send and receive payments instantly, without paying fees for every transaction on the blockchain.

Instead of writing every transaction into the blockchain, the system uses **payment channels** between nodes. These channels allow users to make thousands of offchain payments that are fast, secure, and cost nothing — until it becomes necessary to settle onchain.

---

## How It Works

The network is made up of **payment nodes**. These are programs that connect to each other, create channels, and exchange payments.  
Nodes can be run by individual users or businesses. They can also be integrated into apps and services like TON Storage or TON Proxy to allow automated payments.

There are two types of channels:

- **Onchain channels**: These are smart contracts deployed to the TON blockchain. Two nodes each deposit coins into the contract and create a trusted connection. This is the foundation of the network.
  
- **Virtual channels**: These are temporary, offchain paths created through existing onchain channels.  
  For example, if Alice has a channel with Bob, and Bob has one with Charlie, then Alice can send money to Charlie through a **virtual channel**: `Alice → Bob → Charlie`.

No new smart contracts are needed for this — it’s fast and completely offchain.

---

## Security and Conditional Payments

Payments through virtual channels are protected by **cryptographic guarantees**. When a user sends money, they don’t transfer coins immediately. Instead, they give the middle node a **signed promise**, which can only be completed if the next node confirms receipt.

This is possible thanks to **conditional payments**, where each node in the chain only forwards the value if the next node confirms everything is correct.  

If one of the nodes misbehaves — for example, they break the agreement or stop responding — the system can **fall back to the blockchain**, submit the cryptographic proof, and recover the funds. No one can steal or lose money.

---

## Privacy by Design

The full path of a payment is only known to the sender who builds the route.  
Each middle node knows only:

- Who sent the request
- Who they should forward it to

To protect this structure, the payment path is wrapped in layers of encryption (like **Garlic routing**), so no node can see the full chain.

Dummy data is also added to make it harder to analyze the path.

---

## Performance and Scalability

- Opening and using virtual channels takes only **milliseconds**
- All important operations are **queued and stored safely** using a fast embedded database
- The system is written in **pure Go**, which means it’s **cross-platform** and runs anywhere — even on mobile

---

TON Payment Network combines the speed of offchain payments with the security of blockchain.  
It’s designed for a world where fast, private, and cheap payments are the default.

# Technical Details

The network consists of **peer-to-peer payment nodes** that establish connections between each other via:

- **Smart contract deployment** to the blockchain
- **RLDP protocol** for network communication

A payment node can be:

- A **standalone service** — if the main goal is to **earn fees** from serving virtual channel chains
- A **library integrated into other apps** — if the goal is to **pay or get paid for services**, like **TON Storage** or **TON Proxy**

---

## Example Interactions

![Diagram](https://github.com/xssnick/ton-payment-network/assets/9332353/c127d64f-2f04-4e70-87e6-252d08d1ce47)

---

## Onchain Channels

The onchain channel contract supports:

- **TON coins**
- **Jettons**
- **ExtraCurrencies**

### Channel Discovery

Discovery works by:

- **Scanning new blocks** in the network to catch contract updates
- Or **listening to contract events** (for lite embedded nodes)

For example:

- Detecting deployment of new contracts with known keys when someone wants to establish a channel
- Catching events related to **uncoordinated closures**

### Establishing a Channel

If a node wants to connect with another node:

1. It connects to other node to check if it exists, and reach agreement on future channel configuration. (e.g. closure timeframe, misbehaviour fines)
2. It **deploys a payment channel contract** to the blockchain.
3. The contract includes **two public keys**: the node's own key and the target node’s key.
4. The target node detects the contract, verifies its parameters, and if everything is correct — **allows channel specific communication** after authentication.

### Authentication

For network authentication:

- **Channel keys** are used.
- A special **authentication message** is created from the **ADNL addresses** of both parties and a **timestamp**, signed with the **channel key**.
- The response must include:
    - The **reversed ADNL addresses**
    - The **same timestamp**
    - A **valid signature** from the other party

## Virtual Channels

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

### Security: Why can't `B` steal `A`'s funds?

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

#### Rollbacks and Fallbacks

- If **any node** in the chain refuses to open the channel with the next node, the **entire channel is rolled back** and capacity is unlocked for the sender.
- In rare worst-case scenarios (e.g., a node misbehaves or fails to respond), the capacity will be automatically **unlocked after the channel’s lifetime expires**.

### Safety Guarantees

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

### Onchain Uncooperative Close

An **uncooperative channel close** happens when one of the parties becomes unresponsive or refuses to close the channel properly.  
This process is handled fully onchain and consists of **four key stages**:

1. **Initiation**  
   One of the parties sends an `uncooperative close` transaction to the onchain contract —  
   for example, if the counterparty hasn’t responded for a long time.

2. **Quarantine Phase**  
   Once the transaction is committed, the channel enters a `quarantine` period.  
   During this time, both parties can submit their **latest known state** of the channel.
  - If one party submits an **older state**, the other party can provide a **newer one**.
  - In such a case, the first party is considered dishonest and will be **fined**.

3. **Settlement Phase**  
   After the quarantine period ends, the `settlement` phase begins.  
   Each party can now submit **resolutions of active virtual channels**, if available.  
   This ensures that all pending offchain payments are properly accounted for.

4. **Final Closure**  
   When the settlement is complete, any side could send a final closure signal to the contract.  
   The onchain channel is closed, both sides receive their remaining balances,  
   and the contract becomes **empty and finalized**.

This mechanism ensures fair handling of disputes and protects both parties from data manipulation or malicious behavior.

#### Virtual Channels and Onchain Settlement

- **The number of virtual channels per one onchain channel** is theoretically unlimited.  
  This is achieved by using Merkle proofs to commit conditionals onchain.

- **The channel stores only the Merkle tree root hash** and updates it after each transaction during the settlement phase.

- **Settlement can be performed through any number of transactions.**  
  Each transaction carries a Merkle proof that includes only the conditions to be resolved in that transaction.

- **After a conditional is resolved**, its corresponding value in the dictionary tree is replaced with an empty cell,  
  and the Merkle root hash is updated.  
  This ensures that no conditional can be resolved more than once.

### Privacy of the Virtual Channel

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

### Networking Interactions in a Chain

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

#### Distributed Lock for Channel State Changes

For actions that modify the channel state, a **distributed lock** mechanism is used.

- The `left` node is considered the **lock master**.
- When the `right` node wants to change the channel state, it must **request the `left` node** to lock the state and **wait for the changes**.
- The `left` (master) node can lock the channel **by itself**, as long as it is not already locked by the `right` node.

This mechanism is necessary to **prevent concurrent modifications** and **avoid potential rollback issues**.

It is designed for safety, but can be **optimized later** if performance becomes a concern.

## Speed, Reliability, and Cross-Platform

Opening a virtual channel, while technically complex, is **very fast** — it takes only **milliseconds** to process an action on the node side, even on basic hardware.  
Performance can be further improved in the future with **enhanced lock separation**.

All critical operations are executed through a **special action queue**, which is processed **transactionally** (ACID-compliant) and commits to disk reliably.  
The current database implementation is based on **LevelDB**.

The entire implementation is written in **pure Golang**, making it **cross-platform** — including support for **mobile platforms**.

---

### Standalone Node Management and First Startup

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

