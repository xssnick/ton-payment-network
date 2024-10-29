# TON Payment Network

This is an implementation of a peer-to-peer payment network with multi-node routing based on the power of the TON Blockchain. More powerful than lightning!

## Техническое описание

Сеть состоит из одноранговых самостоятельных платежных узлов, которые устанавливают связи между собой с помощью деплоя смарт контракта в блокчейн и по сети, используя RLDP.

Платежная нода может быть как самостоятельным сервисом, если основная цель - заработок на обслуживании цепочек виртуальных каналов, так и частью других приложений (в виде библиотеки), если цель - предоставление или оплата услуг, например TON Storage и TON Proxy.

Пример взаимодействия: 
![Untitled Diagram drawio(3)](https://github.com/xssnick/ton-payment-network/assets/9332353/c127d64f-2f04-4e70-87e6-252d08d1ce47)


### Onchain каналы

Каждая нода обязательно отслеживает новые блоки в сети и вылавливает обновления, связанные с ее контрактами. Например, появление новых контрактов с ее ключом, когда кто-то хочет установить канал, и события, связанные с несогласованным закрытием.

Если нода хочет установить связь с другой нодой, она деплоит контракт платежного канала в блокчейн. Контракт содержит 2 публичных ключа: свой и соседский. Другая нода обнаруживает контракт в сети, проверяет параметры, и, если все в порядке, позволяет установить с собой сетевое соединение.

Для аутентификации по сети используются ключи каналов, специальное аутентификационное сообщение формируется из adnl адресов сторон и временной метки и подписывается ключем канала, ответное сообщение должно содержать adnl адреса помененые местами, временную метку и подпись другой стороны.

### Виртуальные каналы

Виртуальный канал может быть открыт из любой точки сети в любую другую, если существует цепочка связей между нодами, включая ончеин контракт и активное сетевое соединение. При этом для создания и закрытия виртуального канала не требуется никаких действий ончеин.

Виртуальный канал имеет такие характеристики, как: 
* Ключ
* Время жизни 
* Емкость
* Комиссия
  
Например, `A`, `B` и `C` имеют открытые каналы в блокчейне (`A->B`, `B->C`), при этом не существует ончеин канала `A->C`, но `A` может создать виртуальный канал до `C`, попросив `B` проксировать через его канал за небольшое вознаграждение. Таким образом цепочка будет `A->B->C`.

Глядя на пример выше, возникает вопрос - а что, если `B` возьмет монеты у `A` и не передаст их `C`? 
Ответ: `B` не сможет это сделать благодаря эллиптической криптографии и гибкому блокчейну TON.

Когда `A` просит `B` открыть виртуальный канал, он не передает деньги сразу, а лишь передает `B` подписанную гарантию того, что если `C` предоставит подтверждение получения перевода от `A`, то `B` переведет `C` запрошенную сумму. Затем `B` может запросить у `A` ту же сумму + комиссию, по тому же подтверждению, которое получил от `C`. И так далее по цепочке, если ее длина больше 3.

Каждое звено цепи открывает виртуальные каналы друг с другом, начиная от инициатора и заканчивая точкой назначения. Условия могут изменяться в зависимости от договоренностей между нодами, но ключ всегда остается единым, что позволяет закрыть канал всем участникам, передав подтверждение по цепочке. Условия каскадны от отправителя к получателю и включают друг друга. Например, если в цепочке из 4 звеньев 2 берут комиссию в размере 0.01 TON, то отправитель отправит 0.02 TON комиссии, половину из которой следующее звено передаст дальше. Время жизни канала всегда уменьшается от отправителя к получателю, чтобы предотвратить обман ноды, который заключается в том, чтобы закрыть канал в последний момент, чтобы нода не успела закрыть свою часть с следующим соседом.

В случае, если одна из нод по пути не согласится открыть канал со следующей, канал будет откачен по цепочке назад, и емкость канала будет разблокирована для отправителя. В худшем случае может произойти так, что одна из нод будет действовать не по правилам и не согласится откатывать или не будет отвечать на открытие канала. В таком случае емкость будет разблокирована после указанного в канале времени жизни.

#### Гарантии безопасности

Весь процесс происходит без взаимодействия с блокчейном, следовательно, комиссия сети не платится. К взаимодействию с блокчейном приходится прибегнуть только в случае разногласий, например, если сосед по цепочке ведет себя не по правилам, отказывается передать монеты в обмен на доказательство. Тогда можно просто отправить это доказательство в контракт, закрыв его, и получить свои деньги - все застраховано.

Виртуальные каналы реализованы с помощью условных платежей, условия которых описаны следующей логикой:
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

Логика условных платежей выполняется оффчеин в случае согласованности сторон, а в случае разногласий - ончеин.

#### Анонимность виртуального канала

Все звенья цепи известны только создателю виртуального канала, так как он формирует цепочку. Остальные звенья цепи знают только тех, кто открыл канал с ними, и тех, с кем нужно открыть виртуальный канал им. При этом не получится напрямую идентифицировать, является ли отправитель или получатель конечным звеном или промежуточным.

Это достигается за счет того, что отправитель формирует цепочку в 'Garlic' виде, где задания упакованы и зашифрованы shared ключом и могут быть расшифрованы только тем, кому это задание предназначено. В добавок к реальным заданиям передаются несуществующие для массовки.

Задание состоит из описания того, что должна получить нода от предыдущего соседа, и что передать следующему. Соседи не смогут обмануть друг друга, так как ожидаемые значения описаны в задании, и канал просто отклонится при нарушении.

### Взаимодействия по сети в цепочке

Сетевое взаимодействие строится на двух базовых действиях - `ProposeAction` и `RequestAction`.

* `Propose` - предполагает передачу подписанного модифицированного стейта канала с описанием желаемого изменения, например, открыть виртуальный канал. Сосед может либо принять, либо отказаться. В случае отказа он обязан подтвердить свой отказ подписью. Каждое действие `Propose` транзакционно и должно выполниться полностью или откатиться на обоих сторонах. В случае сетевых ошибок действие повторяется, пока не будет либо принято, либо отказано с подписью. Все действия выполняются строго последовательнов рамках канала. 

* `Request` - запрашивает соседнюю ноду сделать `Propose`, например, закрыть виртуальный канал.

### Скорость, надежность и кроссплатформенность

Открытие виртуального канала, при всей сложности - очень быстрое. Обработка на действия на стороне ноды занимает примерно 3 миллисекунды на обычном рабочем компьютере. А это значит что сервер вполне может открывать и закрывать > 300 виртуальных каналов в секунду, без особого труда. Этот показатель может быть сильно увеличен в дальнейшем, при улучшенном разделении блокировок.

Все важные действия выполняются с помощью специальной очереди, запись в которую происходит транзакционно с другими действиями, и подтверждением коммита на диск (ACID). Текущая реализация базы данных построена поверх встроенной LevelDB. 

Реализация выполнена на чистом Golang, и код может быть скомпилирован под все платформы, включая мобильные.

### Управление нодой

При запуске указывается флаг `-name {seed}`, где `{seed}` это любое слово из которого сгенерируется приватный ключ и кошелек, адрес кошелька выведется в консоль, и его нужно пополнить тестовыми монетами перед дальнейшими действиями.

На данный момент нода в виде самостоятельного сервиса поддерживает несколько консольных команд:

* `list` - Отобразить список активных ончеин и виртуальных каналов.
* `deploy` - Задеплоить канал с нодой имеющей введенный далее ключ (след командой) и балансом.
* `open` - Открыть виртуальный канал с введенным далее ключем, используя введенный далее ончеин канал как тунель. Генерирует и возвращает приватный ключ для вирт канала.
* `send` - Отправить монеты используя самозакрывающийся, после инициализации цепочки, виртуальный канал. Параметры аналогичны `open`.
* `sign` - Принимает на вход приватный ключ виртуального канала и сумму, возвращает стейт в hex формате, который другая сторона может использовать для закрытия виртуального канала.
* `close` - Закрыть виртуальный канал, на вход просит стейт от sign. Закрывать должен получатель.
* `destroy` - Закрыть ончеин канал с указаным далее адресом, сначала пытаемся кооперативно, если не получается, самостоятельно.

Также имеется развернутая нода с которой можно взаимодействовать, ее ключ публичный ключ - `fdf66ea12228f2dab720d3f4deffc82d8a10eef7400ff604aa5d4e7e80758370`

### HTTP API

Нодой можно управлять программно, через API, ниже приведено описание поддерживаемых методов

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

* Открытие виртуального канала с кем-то без кошелька в сети (для перевода ему коинов до деплоя контракта)
* Виртуальные каналы в виде MerkleProof для поддержки практически безлимитного количества активных виртуальных каналов на ончеин канал.
* Обновление состояний через MerkleUpdate.
* Поддержка Postgres в качестве альтернативного хранилища данных.
* API и Webhook события