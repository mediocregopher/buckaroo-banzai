# buckaroo-banzai

A stupid slack bot for printing ~fake~ very real money.

## How it works

Buckaroo Banzai acts as the central bank for your very own crypto token. For
this document we'll use `CRYPTICBUCK`, but you can name it whatever you like.
Buckaroo can create, transfer, withdraw, and deposit these tokens into your
"bank" account, which can be accessed via slack.

The tokens are printed from thin air, and are worth only what you believe they
are (so, quite a lot!)

### Token creation

Buckaroo will listen in on all slack channels he's invited to, and everytime he
sees an emoji reaction has been added to someone's message, he creates and gives
that person one token.

![The birth of a token](docs/img/creation.png?raw=true "The Birth of a Token")

### Token transfer

Tokens can be transferred between slack users without involving the stellar
blockchain.

![The transfer of a token](docs/img/transfer.png?raw=true "Token Transfer")

### Token Withdrawal

Tokens are kept in the slack bank until a user decides to withdraw them. Upon
doing so the tokens are removed from the bank and issued to the stellar address
the user specified.

![The withdrawal of some tokens](docs/img/withdrawal.png?raw=true "Token Withdrawal")

([This](https://horizon.stellar.org/transactions/9216190cc4b1a3e89f427adc1bbd58254168adb013fdac1c6ff3579a5339cb9d)
is that transaction.)

### Token Deposit

Once withdrawn, tokens can be deposited back into the slack bank by sending them
to a federated stellar address.

Here's what it looks like when a deposit has been successful:

![The deposit of a token](docs/img/deposit.png?raw=true "Token Deposit")

## Installation

Clone the repo and `go build ./cmd/buckaroo-banzai`, or use the
`mediocregopher/buckaroo-banzai` docker image.

## Running your own Buckaroo instance

There's a bit of setup involved, you'll need:

* One stellar address, and its seed, with a few lumens (XLM) already deposited.
  XLM is used to pay network fees when someone withdraws their tokens. The fees
  aren't very much, so a few XLM should last a while.

* A redis instance (or two) set up.

* A domain pointed at the IP address Buckaroo will be listening on. This will be
  your [stellar federation
  domain](https://www.stellar.org/developers/guides/concepts/federation.html).

* A slack token for the bot.

* An open heart.

Buckaroo is configured either via CLI or environment variables. Here are the
options used for the bot we run:

```
./buckaroo-banzai \
    --currency-name CRYPTICBUCK \
    --currency-emoji ":crypticbuck:" \
    --slack-token xxx \
    --stellar-seed xxx \
    --stellar-token-name CRYPTICBUCK \
    --stellar-http-net-listen-addr :8000 \
    --stellar-domain xxx.com \
    --stellar-live-net
```

See the `./buckaroo-banzai -h` output for descriptions of the options, and more
available options as well.

### Note about Redis

Buckaroo Banzai needs at least one running redis instance to function, and by
default will try to connect to one over localhost. There are actually two
different configuration parameters for redis addresses, `--bank-redis-addr` and
`--stellar-redis-addr`, corresponding to the different components of Buckaroo
which need redis for independent purposes. You may create two separate redis
instances for these components, or have them use the same one, it's up to you.

# stellar-cli

Since stellar is a bit of a pain to work with, especially on linux where there's
currently no good clients which can connect to the test net, I developed a small
utility called `stellar-cli` which can do most necessary stellar operations from
the command-line.

`stellar-cli` stores no state locally, and is completely configured via
command-line or environment variables. It may be useful at least for generating
a new stellar seed for use with Buckaroo.

You can build `stellar-cli` by cloning the repo and `go build
./cmd/stellar-cli`.
