# slotctl

並列のsession（人・Codex・Claude Codeとそのサブエージェント）が、開発用の実行環境（DB・server・Simulatorなど）を「枠」として借りて、期限つきで使い、返すためのCLIである。

## なぜ必要か

### 背景

AIのcoding agentを並列に動かして開発すると、1台のmachineで、いくつものsessionが同時に作業する。各sessionは、git worktreeで作業場所を分ける。作業場所は安く増やせるが、動かして確かめるための実行環境（DB・Web server・Simulatorなど）は重い。1組で数GBのmemoryと、数十秒の起動時間を使う。

### 問題

実行環境を「worktreeごとに1組」持たせると、次のことが起きる。

- **増え続ける。** worktreeは、agentのツールが作ったり消したりする。sessionの数だけ実行環境が生まれ、上限が無い。
- **終わりが分からず、残る。** sessionのアーカイブ、agentの強制終了、machineのスリープでは、終わりの知らせが届かないことがある。知らせを頼りに片付けると、持ち主のいない実行環境が残り、memoryとportを使い続ける。
- **後から探して片付ける仕組みが膨らむ。** 残った物を見つけて回収する処理を足すと、ツールの振る舞いが変わるたびにどこかが漏れ、直しが続く。

### 解決すること

slotctlは、実行環境を「worktreeの持ち物」から「machineが持つ、数の決まった枠」に変える。

- **数に上限がある。** 枠の数はprojectごとに決める。実行環境は、枠の数より増えない。
- **終わりの知らせに頼らない。** 借りた枠には期限があり、生存連絡（hookから`renew`）で延びる。連絡が途絶えれば、期限切れとして次の人に譲られる。sessionが知らせずに消えても、枠は必ず戻る。
- **期限切れでも作業を失いにくい。** 期限切れの枠は、中身を残したまま「譲れる」状態になるだけ。他に借りたい人が来るまでは、元の借り手がそのまま使い続けられる。
- **何が起きたかを後から確かめられる。** 貸し出し・返却・譲渡を記録に残す。

## しくみ

- projectごとに数の決まった「枠」がある。sessionはCLIで枠を借りる。
- 枠は「pool」（枠の種類）ごとに持つ。DB・serverのような主な枠は「既定のpool」、tunnelや課金の検証のような別の資源は「名前つきのpool」で持ち、数もcommandも別に決める。
- 期限は生存連絡（`renew`）で延びる。途絶えると期限切れになり、譲れる状態になる。
- 期限切れは中身を消さない。空きが無いときに、期限切れのうち最も古いものから譲る。
- 枠の中身は、projectのcommand（`up`・`down`）が作る。slotctlはDB・Docker・Simulatorを知らない。
- 状態はmachineに1つのSQLiteにあり、出来事は記録（JSON Lines）に残る。

単一のGo binaryで、SQLiteはpure-Go（`modernc.org/sqlite`）。

## 安全に使うために

- **信頼できるrepositoryの中でだけ使う。** `acquire`・`release`は、cwdから上の階層で見つけた`slotctl.toml`の`up`・`down`を、`sh -c`でそのまま実行する（npmのscriptsやMakefileと同じ性質）。信頼できないrepositoryの中で打つと、そのrepositoryが書いたcommandが、あなたの権限で動く。
- **秘密はmachineの設定に置き、記録には残らない。** machineの設定の`env`に書いた値は、commandのenvとして渡すだけで、記録（`events-*.jsonl`）には書かない。commandの出力も記録には入らない（画面のstderrへ流れる）。
- **状態と記録は、自分だけが読める。** 状態のdirectoryは`0700`、`state.db`と記録のfileは`0600`で作る（借り手のpathなどを、同じmachineの他の利用者に見せないため）。

## 入れ方

[Releases](https://github.com/taknya/slotctl/releases)に、darwin・linux（arm64・amd64）のbinaryを置いている。Goは要らない。

miseで版を固定するなら、repositoryの`mise.toml`に書く。

```toml
[tools]
"github:taknya/slotctl" = "0.2.0"
```

Goがあれば`go install`でも入る。

```sh
go install github.com/taknya/slotctl/cmd/slotctl@latest
```

`v*`のtagをpushすると、CI（`.github/workflows/release.yml`）がtestを通してからbinaryを作り、Releaseに置く。

slotctl自体の開発では、Goの版は`mise.toml`が決める。手元で試すときは次を使う。

```sh
mise install
mise exec -- go build -o slotctl ./cmd/slotctl
```

## 用語

| 語      | 意味                                                                                               |
| ------- | -------------------------------------------------------------------------------------------------- |
| project | `slotctl.toml`の`project`。枠を共有する単位。                                                      |
| pool    | 枠の種類。既定のpool（`default`）と、名前つきのpool。枠の数・port・`up`・`down`はpoolごとに持つ。 |
| 枠      | poolが持つ、番号（1から）のついた使い場所。名前は`<project>-<slot>`（名前つきpoolは`<project>-<pool>-<slot>`）。 |
| holder  | 枠の借り手。cwdのgit worktreeの実path（`git rev-parse --show-toplevel`）。gitの外ならcwdの実path。 |
| lease   | holderが枠を借りている記録。期限（`expires_at`）を持つ。holderは、1つのpoolで1枠まで借りられる。   |

## 設定

### projectの設定（`slotctl.toml`）

cwdから上へ辿って最初に見つかる`slotctl.toml`を使う。repositoryにcommitする。

```toml
project = "myapp"

[lease]
ttl = "10m"                  # 期限。生存連絡（renew）で「今＋ttl」になる。既定10m

[slots]
count = 3                    # 枠の数。既定1、最大10

[ports]
names = ["web", "db"]        # 枠ごとに割り当てるportの名前（順番が番号になる）

[commands]
up   = "bin/dev slot up"     # 枠を使える状態にする
down = "bin/dev slot down"   # 枠を止める

# 名前つきのpool（省略できる。詳しくは「pool」）
[pools.billing]
count = 1                    # 枠の数。既定1、最大10
up    = ""                   # 省略できる
down  = "bin/dev billing down"
ports = []                   # 省略できる。portの名前
```

`[slots]`・`[ports]`・`[commands]`は、既定のpoolの設定である。`up`・`down`は省略できる。未知のkeyはエラーにする。

### machineの設定（任意）

`$XDG_CONFIG_HOME/slotctl/config.toml`（既定`~/.config/slotctl/config.toml`）。

```toml
port_start = 12000            # projectの帯を割り当て始めるport（既定12000）
log_retention_months = 3      # 記録を残す月数（既定3）

[projects.myapp]
slots = 3                     # 既定のpoolのcountを上書き
env = { MYAPP_HOME = "~/Development/myapp-local-state" }   # commandに渡すenv（先頭の~はHOMEに展開）

[projects.myapp.pools.billing]
slots = 2                     # 名前つきpool（billing）のcountを上書き
```

`slotctl.toml`に無いpoolの上書きは、無視する。未知のkeyはエラーにする。

### 置き場

| 種類               | 置き場                                                      |
| ------------------ | ----------------------------------------------------------- |
| 状態（`state.db`） | `$XDG_STATE_HOME/slotctl/`（既定`~/.local/state/slotctl/`） |
| 記録               | 状態と同じdirectoryの`events-YYYY-MM.jsonl`                 |
| machineの設定      | `$XDG_CONFIG_HOME/slotctl/config.toml`                      |

環境変数`SLOTCTL_HOME`を設定すると、状態・記録・machineの設定の置き場をそのdirectory1つに差し替える（testや試用向け）。

## port

portは、poolごとの帯から割り当てる。既定のpoolの帯は、projectが初めて使われたときに、`port_start`から1000ずつ空いている帯を割り当てて記録する。名前つきpoolの帯は、そのpoolがportの名前（`ports`）を持つときだけ、初めて使われたときに、machineで空いている帯から割り当てて記録する。portは次のとおり。

```text
port = 帯の先頭 + 100 × (slot − 1) + portの名前の順番
```

同じpoolの帯は変わらず、pool・projectどうしの帯は重ならない。portの名前を持たない名前つきpoolは、帯もportも持たない。同じ`project`名を別のrepository（git common dir。gitの外なら`slotctl.toml`のあるdirectory）が名乗ると、命令は失敗する。

## 命令

```text
slotctl acquire [pool] [--json]   poolの枠を借りる（省略すると既定のpool）
slotctl renew                     holderの全poolのleaseの期限を延ばす
slotctl release [pool]            poolの枠を返す（省略するとholderの全poolの枠）
slotctl status [--json]           全poolの全枠の状態を出す
```

`pool`に設定に無い名前を渡すと、終了code 2で失敗する。`default`は既定のpoolの名前として渡せる。

### acquire

1つのtransaction（`BEGIN IMMEDIATE`）で、poolの中で次の順に枠を決める。

1. 同じholderのそのpoolの枠があれば、それを延長して返す（冪等。`up`は走らせない）。
2. 無ければ、空き枠（lease無し）の最も小さい番号。
3. 空きが無ければ、期限切れのうち`expires_at`が最も古い枠を譲る（前のholderを記録する）。
4. どれも無ければ、終了code 3。そのpoolを使っているholderと期限を出す。

同じholderは、別々のpoolの枠を同時に借りられる（1つのpoolでは1枠まで）。

transactionの外で、新しく借りた場合にそのpoolの`up`を走らせる。譲った場合は、先にその枠のpoolの`down`を前のholderのenvで走らせ、次に`up`を走らせる。`up`が失敗したらleaseを消して失敗を返す（前のholderの`down`の失敗は警告と記録にとどめ、`up`へ進む）。

出力は、slot・名前（`<project>-<slot>`、名前つきpoolは`<project>-<pool>-<slot>`）・port・期限。`--json`ならJSON（`pool`を含む）。

```text
$ slotctl acquire
slot: 1
name: myapp-1
port: web=12000 db=12001
expires: 2026-09-29T14:10:00+09:00
```

### renew

holderの全poolのleaseの`expires_at`を「今＋ttl」にする（ttlは全poolに共通）。leaseが無ければ何もせず0で終わる。commandは走らせず、記録も書かない。hookから毎回呼ばれるので速く保つ。`slotctl.toml`が無い場所でも何もせず0で終わる。

### release

poolを指定すればそのpoolの、省略すればholderの全poolの枠を返す。返す枠ごとに、そのpoolの`down`を走らせてからleaseを消す。返す枠が無ければ何もせず0で終わる。どれかの`down`が失敗しても、全てのleaseを消し、失敗を記録して終了code 1にする。`slotctl.toml`が無い場所では何もせず0で終わる。`slotctl.toml`から消えたpoolの枠は、`down`を走らせずにleaseだけを消す。

### status

projectの全pool（既定のpool、続いて名前つきpoolの名前順）の全枠について、pool・slot・名前・状態・holder・期限・portを出す。`--json`の各要素には`pool`（既定のpoolは`"default"`）がある。状態は次のとおり。

| 状態      | 意味                                 |
| --------- | ------------------------------------ |
| `lent`    | 貸し出し中で、期限内                 |
| `expired` | 貸し出したままで、期限切れ（譲れる） |
| `free`    | lease無し                            |

## commandに渡すenv

`up`・`down`は、そのpoolのcommandである。holderのworktreeをcwdにして`sh -c`で走る。commandの出力はstderrへ流れる（`acquire --json`のstdoutを汚さない）。

| 名前                          | 値                              |
| ----------------------------- | ------------------------------- |
| `SLOTCTL_PROJECT`             | project名                       |
| `SLOTCTL_POOL`                | poolの名前（既定のpoolは`default`） |
| `SLOTCTL_SLOT`                | 枠の番号                        |
| `SLOTCTL_NAME`                | 枠の名前（`<project>-<slot>`、名前つきpoolは`<project>-<pool>-<slot>`） |
| `SLOTCTL_HOLDER`              | holderのpath                    |
| `SLOTCTL_PORT_<名前の大文字>` | そのpoolのport。例：`SLOTCTL_PORT_WEB` |
| machineの設定の`env`          | `[projects.<name>] env`の各項目 |

譲る前のholderの`down`には、そのholderの値が渡る。

## 終了code

| code | 意味                                                                        |
| ---- | --------------------------------------------------------------------------- |
| 0    | 成功                                                                        |
| 1    | その他の失敗（`up`の失敗、`release`の`down`の失敗、設定・DBの誤りなど）     |
| 2    | 使い方の誤り（未知の命令・flag・pool、`acquire`・`status`で`slotctl.toml`が無い） |
| 3    | poolに空きが無い（`acquire`）                                               |

## 記録

`$XDG_STATE_HOME/slotctl/events-YYYY-MM.jsonl`に、JSON Linesで追記する。

```json
{"at":"2026-09-29T14:00:00+09:00","event":"preempt","project":"myapp","pool":"default","slot":3,"holder":"/work/b","previous_holder":"/work/a"}
{"at":"2026-09-29T14:00:01+09:00","event":"down","project":"myapp","pool":"default","slot":3,"holder":"/work/a","ok":true,"ms":820}
{"at":"2026-09-29T14:00:05+09:00","event":"up","project":"myapp","pool":"default","slot":3,"holder":"/work/b","ok":true,"ms":4100}
{"at":"2026-09-29T14:00:05+09:00","event":"acquire","project":"myapp","pool":"default","slot":3,"holder":"/work/b","ok":true,"ms":4990}
```

| 項目                | 意味                                                               |
| ------------------- | ------------------------------------------------------------------ |
| `at`                | 時刻（RFC 3339）                                                   |
| `event`             | `acquire`・`preempt`・`release`・`up`・`down`。`renew`は書かない   |
| `project`・`pool`・`slot` | 対象の枠。空きが無くて借りられなかったときは`slot`が無い     |
| `holder`            | そのeventの主（`down`は、その`down`を受けるholder）                |
| `previous_holder`   | `preempt`だけ。譲られた前のholder                                  |
| `ok`・`ms`・`error` | 成否・所要ミリ秒・失敗の内容（`acquire`・`up`・`down`・`release`） |

`acquire`は、新しく借りたとき（延長ではないとき）と、空きが無くて借りられなかったときに書く。書くときに、今月を含む直近`log_retention_months`か月より古い`events-*.jsonl`を消す。

## pool

poolは、枠の種類である。「DBとweb serverの組」のような主な枠は既定のpoolに、tunnelや課金の検証のような別の資源は名前つきのpoolに分けると、資源ごとに数も`up`・`down`も別にでき、1つのsessionが両方を同時に借りられる。poolの意味は変わらない。数の決まった枠を、期限つきで貸す。

### 設定

```toml
project = "myapp"

[lease]
ttl = "10m"                      # 全poolに共通

# 既定のpool
[slots]
count = 3
[ports]
names = ["web", "api"]
[commands]
down = "bin/dev slot down"

# 名前つきのpool
[pools.billing]
count = 1                        # 既定1、最大10
down = "bin/dev billing down"    # up・down・portsは省略できる
ports = []

[pools.tunnel]
count = 1
```

- poolの名前は`^[a-z][a-z0-9-]*$`。`default`は既定のpoolの名前なので、`[pools.default]`は設定の誤りになる。
- 名前つきpoolの`up`・`down`は、そのpoolだけのcommandである。`[commands]`は引き継がない。
- machineの設定では、`[projects.<project>] slots`が既定のpoolの、`[projects.<project>.pools.<pool>] slots`が名前つきpoolの数を上書きする。
- 未知のkeyは、`slotctl.toml`もmachineの設定もエラーにする。

### 名前・port・env

| 項目               | 既定のpool               | 名前つきのpool                   |
| ------------------ | ------------------------ | -------------------------------- |
| 枠の名前           | `<project>-<slot>`       | `<project>-<pool>-<slot>`        |
| portの帯           | 既定の帯（従来どおり）   | portの名前を持つpoolだけ、専用の帯 |
| `SLOTCTL_POOL`     | `default`                | pool名                           |
| `SLOTCTL_PORT_*`   | 既定のpoolのport         | そのpoolのport                   |

### 使い方

```text
$ slotctl acquire billing
slot: 1
name: myapp-billing-1
port:
expires: 2026-09-29T14:10:00+09:00

$ slotctl status
POOL     SLOT  NAME             STATE  HOLDER   EXPIRES                    PORT
default  1     myapp-1          lent   /work/a  2026-09-29T14:10:00+09:00  web=12000 api=12001
billing  1     myapp-billing-1  lent   /work/a  2026-09-29T14:10:00+09:00
```

### 状態の引き継ぎ

state.dbのschemaは版2で、leaseを`(project, pool, slot)`で持つ。v0.1.0のstate.dbを開くと、既存のleaseとport帯が既定のpoolのものとして引き継がれる（貸し出し中の枠は失われない）。移行は1つのtransactionで行う。移行後のstate.dbは、v0.1.0のslotctlでは使えない。

## やらないこと

- **processの管理**（起動・監視・再起動）。枠の中身は`up`・`down`が作り、slotctlは走らせるだけである。
- **期限の無い保持**。全てのleaseに期限がある。
- **資源ごとの知識**（ngrok・emulator・Dockerなど）。資源の扱いは、poolの`up`・`down`に書く。
- **複数のpoolの枠を、1つのtransactionでまとめて借りること**。まとめて握る物は、1つのpoolにする。
- 待ち行列、自然な片付け（cleanup_after）、worktree消滅時の回収、reset命令、launchd、サブエージェントの判定、machine全体の上限。

## hookの設定例

sessionが動いている間は`renew`で期限を延ばし、終わったら`release`で返す。生存連絡が止まれば、自然に期限切れになる。

### Claude Code（`.claude/settings.json`）

```json
{
  "hooks": {
    "Stop": [{ "hooks": [{ "type": "command", "command": "slotctl renew" }] }],
    "PreToolUse": [
      { "hooks": [{ "type": "command", "command": "slotctl renew" }] }
    ],
    "SessionEnd": [
      { "hooks": [{ "type": "command", "command": "slotctl release" }] }
    ]
  }
}
```

### Codex

Codexのhookでも、Stop・PreToolUse相当のeventに`slotctl renew`を、SessionEnd相当のeventに`slotctl release`を割り当てる。

hookの設定書式は各ツールの版で変わる。eventとcommandの対応（Stop・PreToolUseで`renew`、SessionEndで`release`）を保ち、書式は各ツールの文書に従う。`renew`・`release`は`slotctl.toml`の無いrepositoryでは何もせず0で終わるので、全repositoryに共通のhookとして置いてもよい。

`renew`は借り手の全poolのleaseを延ばし、`release`は全poolの枠を返すので、poolが増えてもhookの設定は変わらない。

`acquire`は、sessionの開始時にsessionが自分で呼ぶ（またはproject側のscriptが呼ぶ）。枠の`up`・`down`が何をするかは、projectの`[commands]`が決める。

## 開発

```sh
mise exec -- go vet ./...
mise exec -- go test ./...
```

```text
cmd/slotctl/        入口（薄い）。同時申請のtestはここでbinaryをbuildして行う
internal/config/    slotctl.tomlの探索、pool、machine設定、SLOTCTL_HOMEとXDGの解決
internal/store/     SQLite（schema・移行・transaction・projects・port帯・leases）
internal/lease/     acquire・renew・release・statusの規則（時刻は注入）
internal/command/   up・downをenvつきで`sh -c`で走らせる
internal/eventlog/  JSON Linesの追記、月ごとのfile、ローテーション
internal/holder/    holderとrepositoryの識別
internal/ports/     portの帯と割り当て
internal/cli/       flag・出力・終了code
```

## ライセンス

[MIT License](LICENSE)。Copyright (c) 2026 taknya
