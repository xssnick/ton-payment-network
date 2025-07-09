import React, {useEffect, useState} from 'react';
import './App.css';
import {TonConnectButton, useTonAddress, useTonConnectUI, useTonWallet} from "@tonconnect/ui-react";

import {Send, ArrowDown, ArrowUp, RefreshCw, Copy, PlusCircle, MinusCircle, Activity, Check, Plus} from "lucide-react";
import {Card, CardContent} from "./components/ui/card";
import {Input} from "./components/ui/input";
import {Button} from "./components/ui/button";
import {PaymentChannelHistoryItem} from "./index";


function App() {
  const [tonConnectUI] = useTonConnectUI();
  const wallet = useTonWallet();
  let addr = useTonAddress();
  let [paymentAddr, setPaymentAddr] = useState("Loading...");
  let [balance, setBalance] = useState("...");
  let [capacity, setCapacity] = useState("...");
  let [history, setHistory] = useState<PaymentChannelHistoryItem[] | null>(null);

  window.onPaymentNetworkLoaded = function(addr) {
    setPaymentAddr(addr);
    console.log("Payment network loaded: "+addr);
  }
  window.onPaymentChannelUpdated = function(ev) {
    setBalance(ev.balance);
    setCapacity(ev.capacity);
    window.getChannelHistory(5).then(history => {
      setHistory(history);
    })
  }

  useEffect(() => {
    if (!wallet) return;

    const initWasm = async () => {
      window.walletAddress = () => {
        return addr;
      };

      window.doTransaction = async (reason, to, amtNano, body, state) => {
        console.log("requested tx: "+ reason);

        const transaction = {
          validUntil: Math.floor(Date.now() / 1000) + 90,
          messages: [
            {
              address: to,
              amount: amtNano,
              stateInit: state,
              payload: body,
            }
          ]
        }

        let resp = await tonConnectUI.sendTransaction(transaction);
        return resp.boc;
      }

      const go = new (window as any).Go();
      const wasmUrl = 'web.wasm';
      let wasmModule;

      if ('instantiateStreaming' in WebAssembly) {
        wasmModule = await WebAssembly.instantiateStreaming(fetch(wasmUrl), go.importObject);
      } else {
        const resp = await fetch(wasmUrl);
        const bytes = await resp.arrayBuffer();
        wasmModule = await WebAssembly.instantiate(bytes, go.importObject);
      }

      go.run(wasmModule.instance);

      const waitForStartPaymentNetwork = (timeoutMs: number = 5000, intervalMs: number = 50): Promise<void> => {
        return new Promise((resolve, reject) => {
          const startTime = Date.now();
          const interval = setInterval(() => {
            if (typeof window.startPaymentNetwork === 'function') {
              clearInterval(interval);
              resolve();
            } else if (Date.now() - startTime > timeoutMs) {
              clearInterval(interval);
              reject(new Error('startPaymentNetwork was not registered in time'));
            }
          }, intervalMs);
        });
      };

      try {
        await waitForStartPaymentNetwork();
        window.startPaymentNetwork("FnfzUIFAsZcxWj5h/8aYP25Zx0MjH+LuRue41LRPGAs=", "3zGuH6bJAPE+4H4I3G5zniPJm4Pjz08H8hOAfdmgcBo=");
      } catch (e) {
        console.error(e);
      }
    };

    initWasm();
  }, [wallet]);

  if (!wallet) {
    return (
        <div className="min-h-screen bg-white text-gray-800 p-6 space-y-6">
          <div className="flex justify-between items-center">
            <h1 className="text-2xl font-bold text-[#0098ea]">TON Payments Wallet</h1>
            <TonConnectButton/>
          </div>
        </div>
    );
  }

  return (
      <WalletUI paymentAddr={paymentAddr} balance={balance} capacity={capacity} transactions={history}/>
  );
}

type WalletUIProps = {
  paymentAddr: string;
  balance: string;
  capacity: string;
  transactions: PaymentChannelHistoryItem[] | null;
};

const WalletUI: React.FC<WalletUIProps> = ({ paymentAddr, balance, capacity, transactions }) => {
  const [connected, setConnected] = useState(false);
  const [sendTo, setSendTo] = useState("");
  const [sendAmount, setSendAmount] = useState("");
  const [copied, setCopied] = useState(false);
  const [modalType, setModalType] = useState<"topup" | "withdraw" | null>(null);
  const [modalAmount, setModalAmount] = useState("");

  const handleCopy = () => {
    if (!paymentAddr) return;
    navigator.clipboard.writeText(paymentAddr);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  const closeModal = () => {
    setModalType(null);
    setModalAmount("");
  };

  const confirmModal = () => {
    if (modalType == "topup") {
      window.topupChannel(modalAmount);
    } else if (modalType == "withdraw") {
      window.withdrawChannel(modalAmount);
    }
    closeModal();
  };

  return (
      <div className="min-h-screen bg-white text-gray-800 p-4 sm:p-6 flex justify-center">
        <div className="w-full max-w-xl space-y-6">
          <div className="flex justify-between items-center">
            <h1 className="text-2xl font-bold text-[#0098ea]">TON Payments Wallet</h1>
            <TonConnectButton />
          </div>

          <Card className="bg-[#f0f8ff] shadow-md rounded-2xl">
            <CardContent className="p-6 space-y-4">
              <h2 className="text-xl font-semibold">Balance</h2>
              <div className="flex items-center justify-between">
                <div className="text-3xl text-[#0098ea]">{balance} TON</div>
                <div className="space-x-2">
                  {balance === "" ? (
                          <Button onClick={()=>{ window.deployChannel() }} className="bg-[#0098ea] text-white px-4 py-2 rounded-xl">Deploy Wallet</Button>
                      ) : (
                          <>
                            <Button onClick={() => setModalType("topup")} className="bg-[#0098ea] text-white px-3 py-1 rounded-lg text-sm">Top Up</Button>
                            <Button onClick={() => setModalType("withdraw")} className="bg-gray-200 text-gray-700 px-3 py-1 rounded-lg text-sm">Withdraw</Button>
                          </>
                      )}
                </div>
              </div>
              <div className="flex items-center justify-between mt-2">
                <span className="text-sm text-gray-500">Capacity</span>
                <span className="text-sm font-medium">{capacity} TON</span>
              </div>

              <h2 className="text-xl font-semibold">Your Address</h2>
              {balance === "..." ? (
                  <div className="text-gray-500 flex items-center gap-2">
                    <RefreshCw className="animate-spin" size={18} /> Loading...
                  </div>
              ) : balance === "" ? (
                  <div className="relative bg-gradient-to-r from-[#f0f8ff] to-white border border-[#cce5ff] rounded-xl px-4 py-3">
                    <div className="text-xs text-gray-700 font-mono truncate pr-10">{"Not deployed"}</div>
                  </div>
              ) : (
                  <div className="relative bg-gradient-to-r from-[#f0f8ff] to-white border border-[#cce5ff] rounded-xl px-4 py-3">
                    <div className="text-xs text-gray-700 font-mono truncate pr-10">{paymentAddr}</div>
                    <button
                        onClick={handleCopy}
                        className="absolute top-1/2 right-3 -translate-y-1/2 text-[#0098ea] hover:text-blue-600"
                    >
                      {copied ? <span className="text-sm animate-pulse">Copied!</span> : <Copy size={16} />}
                    </button>
                  </div>
              )}
            </CardContent>
          </Card>

          <Card className="bg-[#f9fcff] shadow-md rounded-2xl">
            <CardContent className="p-6 space-y-4">
              <h2 className="text-xl font-semibold">Send</h2>
              <Input placeholder="Recipient address" value={sendTo} onChange={(e) => setSendTo(e.target.value)} />
              <Input placeholder="Amount in TON" value={sendAmount} onChange={(e) => setSendAmount(e.target.value)} />
              <Button className="bg-[#0098ea] text-white px-4 py-2 rounded-xl flex items-center gap-2" onClick={()=>{
                let res = window.sendTransfer(sendAmount, sendTo);
                if (res !== "") {
                  console.log("failed to transfer: "+res);
                  return;
                }
                console.log("transferred: "+sendAmount+" to "+sendTo);
              }}>
                <Send size={16} /> Send
              </Button>
            </CardContent>
          </Card>

          {transactions && (
              <Card className="bg-[#f9fcff] shadow-md rounded-2xl">
                <CardContent className="p-6 space-y-4">
                  <h2 className="text-xl font-semibold">History</h2>

                  <div className="space-y-2">
                    {transactions.map((tx) => (
                        <div
                            className="flex justify-between items-center border-b border-gray-100 pb-2"
                        >
                          {/* левая часть: иконка + время */}
                          <div className="flex items-center gap-2">
                            {(() => {
                              const p = { size: 16 };
                              switch (tx.action) {
                                case 1: // Top-up
                                  return <Plus        className="text-green-500" {...p} />;
                                case 2: // Top-up capacity
                                  return <PlusCircle  className="text-green-500" {...p} />;
                                case 3: // Withdraw
                                  return <MinusCircle className="text-red-500"   {...p} />;
                                case 4: // Withdraw capacity
                                  return <MinusCircle className="text-red-500"   {...p} />;
                                case 5: // Transfer-in
                                  return <ArrowDown   className="text-green-500" {...p} />;
                                case 6: // Transfer-out
                                  return <ArrowUp     className="text-red-500"   {...p} />;
                                case 7: // Uncooperative close
                                  return <Activity    className="text-orange-500" {...p} />;
                                case 8: // Closed
                                  return <Check       className="text-gray-500"  {...p} />;
                                default:
                                  return <ArrowDown   {...p} />;
                              }
                            })()}

                            <span className="text-sm text-gray-600">{tx.timestamp}</span>
                          </div>

                          {/* правая часть: сумма + кнопка-адрес */}
                          <div className="flex flex-col items-end">
                            {tx.amount && (
                                <div className="text-sm font-medium">{tx.amount} TON</div>
                            )}

                            {tx.party && (
                                <button
                                    className="group flex items-center gap-1 text-xs text-blue-600 hover:underline"
                                    onClick={() => navigator.clipboard.writeText(tx.party!)}
                                    title="Copy address"
                                >
                                  <Copy
                                      size={12}
                                      className="opacity-70 group-hover:opacity-100"
                                  />
                                  {tx.party.slice(0, 4)}…{tx.party.slice(-4)}
                                </button>
                            )}
                          </div>
                        </div>
                    ))}
                  </div>
                </CardContent>
              </Card>
          )}

          {modalType && (
              <ModalAmount
                  title={modalType}
                  value={modalAmount}
                  onChange={setModalAmount}
                  onConfirm={confirmModal}
                  onCancel={closeModal}
              />
          )}
        </div>
      </div>
  );
};


const ModalAmount: React.FC<{
  title: string;
  value: string;
  onChange: (value: string) => void;
  onConfirm: () => void;
  onCancel: () => void;
}> = ({ title, value, onChange, onConfirm, onCancel }) => (
    <div className="fixed inset-0 bg-black bg-opacity-40 flex items-center justify-center z-50">
      <div className="bg-white p-6 rounded-2xl shadow-xl w-80 space-y-4">
        <h2 className="text-lg font-semibold capitalize text-center">{title}</h2>
        <Input
            type="number"
            step="0.000000001"
            placeholder="Enter amount"
            value={value}
            onChange={(e) => onChange(e.target.value)}
        />
        <div className="flex justify-between gap-4">
          <Button onClick={onConfirm} className="bg-[#0098ea] text-white w-full">Confirm</Button>
          <Button onClick={onCancel} className="bg-gray-200 text-gray-700 w-full">Cancel</Button>
        </div>
      </div>
    </div>
);

export default App;
