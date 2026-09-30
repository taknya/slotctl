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
- **終わりの知らせに頼らない。** 借りた枠には期限があり、ハートビート（hookから`renew`）で延びる。連絡が途絶えた枠は、`acquire`または`reclaim`が中身を止めて空ける。
- **期限切れでも延長できる。** 誰も借りに来ず、`reclaim_after`も過ぎていなければ、期限切れの枠もそのまま使え、ハートビートで延長できる。
- **何が起きたかを後から確かめられる。** 貸し出し・返却・回収を記録に残す。

## しくみ

- projectは、pool（枠の種類）ごとに数の決まった枠を持つ。全poolの名前は`<project>-<pool>-<番号>`である。
- 期限はハートビート（`renew`）で「今＋ttl」に延びる。
- `acquire <pool>`は、同じholderの枠を延長するか、全poolの期限切れを片付けてから最小番号の空き枠を貸す。
- `reclaim`は、全poolで最後のハートビートから`reclaim_after`たった枠を片付ける。
- 枠の中身はprojectのcommandが扱う。slotctlはDB・Docker・Simulatorの知識を持たない。`down`が成功した枠だけを空きにする。
- 状態はmachineに1つのSQLiteにあり、出来事は記録（JSON Lines）に残る。

単一のGo binaryで、SQLiteはpure-Go（`modernc.org/sqlite`）。

## 安全に使うために

- **信頼できるrepositoryの中でだけ使う。** `acquire`・`release`・`reclaim`は、cwdから上の階層で見つけた`slotctl.toml`の`up`・`down`を、`sh -c`でそのまま実行する（npmのscriptsやMakefileと同じ性質）。信頼できないrepositoryの中で打つと、そのrepositoryが書いたcommandが、あなたの権限で動く。
- **秘密はmachineの設定に置き、記録には残らない。** machineの設定の`env`に書いた値は、commandのenvとして渡すだけで、記録（`events-*.jsonl`）には書かない。commandの出力も記録には入らない（画面のstderrへ流れる）。
- **状態と記録は、自分だけが読める。** 状態のdirectoryは`0700`、`state.db`と記録のfileは`0600`で作る（借り手のpathなどを、同じmachineの他の利用者に見せないため）。

## 入れ方

[Releases](https://github.com/taknya/slotctl/releases)に、darwin・linux（arm64・amd64）のbinaryを置いている。Goは要らない。

miseで版を固定するなら、repositoryの`mise.toml`に書く。

```toml
[tools]
"github:taknya/slotctl" = "0.3.0"
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

| 語 | 意味 |
| -- | -- |
| project | `slotctl.toml`の`project`。枠を共有する単位。 |
| pool | 枠の種類。枠の数・port・`up`・`down`をそれぞれ持つ。 |
| 枠 | poolが持つ、1からの番号のついた使い場所。名前は`<project>-<pool>-<番号>`。 |
| holder | cwdのgit worktreeの実path。gitの外ならcwdの実path。 |
| lease | holderが枠を借りている記録。期限を持ち、1つのpoolでは1枠まで借りられる。 |
| ハートビート | `renew`で最後の連絡時刻と期限を更新すること。 |

## 設定

### projectの設定（`slotctl.toml`）

cwdから上へ辿って最初に見つかる`slotctl.toml`を使う。repositoryにcommitする。

```toml
project = "myapp"

[lease]
ttl = "10m"                  # ハートビートで「今＋ttl」にする。既定10m
reclaim_after = "30m"        # 最後のハートビートから回収まで。既定30m

[pools.dev]
count = 3                    # 枠の数。既定1、最大10
ports = ["web", "db"]        # 枠ごとに割り当てるportの名前
# up = "..."                # 任意。新しく借りたときに走るcommand
# 借りるだけにする場合はupを省き、必要なものを利用者が起動する。
down = "bin/dev slot down"   # 枠の中身を全て止める

[pools.billing]
count = 1
down = "bin/dev slot down"
```

poolは`[pools.<名前>]`で宣言する。名前は`^[a-z][a-z0-9-]*$`で、全poolに同じ規則が適用される。`up`・`down`・`ports`は省略できる。`ttl`・`reclaim_after`は1秒以上のdurationで、全poolに共通。未知のkeyは設定の誤りにする。`[slots]`・`[ports]`・`[commands]`は使えない。

### machineの設定（任意）

`$XDG_CONFIG_HOME/slotctl/config.toml`（既定`~/.config/slotctl/config.toml`）。

```toml
port_start = 12000            # projectの帯を割り当て始めるport（既定12000）
log_retention_months = 3      # 記録を残す月数（既定3）

[projects.myapp]
env = { MYAPP_HOME = "~/Development/myapp-local-state" }   # commandに渡すenv（先頭の~はHOMEに展開）

[projects.myapp.pools.billing]
slots = 2                     # billingのcountを上書き
```

`slotctl.toml`に無いpoolの上書きは、無視する。未知のkeyはエラーにする。枠の数の上書きは`[projects.<project>.pools.<pool>] slots`に書く。`[projects.<project>] slots`は使えない。

### 置き場

| 種類               | 置き場                                                      |
| ------------------ | ----------------------------------------------------------- |
| 状態（`state.db`） | `$XDG_STATE_HOME/slotctl/`（既定`~/.local/state/slotctl/`） |
| 記録               | 状態と同じdirectoryの`events-YYYY-MM.jsonl`                 |
| machineの設定      | `$XDG_CONFIG_HOME/slotctl/config.toml`                      |

環境変数`SLOTCTL_HOME`を設定すると、状態・記録・machineの設定の置き場をそのdirectory1つに差し替える（testや試用向け）。

## port

portは、poolごとにmachineで空いている1000個の帯から割り当て、記録する。portの名前を持つpoolだけがportを持つ。

```text
port = 帯の先頭 + 100 × (slot − 1) + portの名前の順番
```

同じpoolの帯は変わらず、pool・projectどうしの帯は重ならない。同じ`project`名を別のrepository（git common dir。gitの外なら`slotctl.toml`のあるdirectory）が名乗ると失敗する。

## 命令

```text
slotctl acquire <pool> [--json]  poolの枠を借りる（pool必須）
slotctl renew                    holderの全poolの期限を延ばす
slotctl release [pool]           枠を返す（省略するとholderの全pool）
slotctl reclaim                  projectの全poolの古い枠を空ける
slotctl status [--json]          全poolの全枠の状態を出す
```

### acquire

1つのtransaction（`BEGIN IMMEDIATE`）で次の順に決める。

1. 同じholderのそのpoolの枠があれば延長して返す。commandも記録も走らせない。
2. projectの全poolの期限切れの枠を、それぞれ元のholderのenvとcwdで`down`して空ける。
3. 指定poolの空き枠から、最も小さい番号を貸す。
4. 空きが無ければ終了code 3。そのpoolのholderと期限を表示する。

`down`が失敗した枠は貸出中のまま保持し、枠の名前と失敗の内容をstderrに出す。他に空きがあればそれを貸す。新しく借りたときだけ、transactionの外で任意の`up`を走らせる。`up`が失敗したらleaseを消して終了code 1で終わる。

```text
$ slotctl acquire dev
slot: 1
name: myapp-dev-1
port: web=12000 db=12001
expires: 2026-09-29T14:10:00+09:00
```

`--json`はproject・pool・slot・name・holder・ports・expires_atを返す。

### renew

holderの全poolの最後のハートビートを今にし、期限を「今＋ttl」にする。期限切れでも延長できる。leaseが無ければ何もせず0で終わる。commandも記録も走らせない。`slotctl.toml`が無い場所でも0で終わる。

### release

poolを指定すればそのpoolの、省略すればholderの全poolの枠を返す。各枠の`down`が成功してからleaseを消す。失敗した枠は保持し、他の枠の返却は続ける。枠の名前と失敗の内容をstderrと記録へ出し、終了code 1で終わる。次の`release`・`acquire`・`reclaim`で再試行できる。設定から消えたpoolのleaseも、`down`を実行できないため保持して知らせる。返す枠が無ければ何もせず0。`slotctl.toml`が無い場所でも0で終わる。

### reclaim

引数は取らない。projectの全poolで、最後のハートビートから`reclaim_after`以上たった枠を、使っている最中でも`down`して空ける。空ける枠が無ければ出力も記録もなく0で終わる。`down`が失敗した枠は保持し、失敗を知らせて終了code 1で終わる。他の枠の回収は続け、次回に再試行する。`slotctl.toml`が無い場所では0で終わる。

### status

全poolの全枠をpool名順・番号順で表示する。状態は次の2つである。期限を過ぎた貸出中の枠も`lent`として、その期限を表示する。

| 状態 | 意味 |
| -- | -- |
| `lent` | 貸出中 |
| `free` | lease無し。中身は止まっている |

## commandに渡すenv

`up`・`down`は、そのpoolのcommandである。holderのworktreeをcwdにして`sh -c`で走る。commandの出力はstderrへ流れる（`acquire --json`のstdoutを汚さない）。

| 名前                          | 値                              |
| ----------------------------- | ------------------------------- |
| `SLOTCTL_PROJECT`             | project名                       |
| `SLOTCTL_POOL`                | poolの名前 |
| `SLOTCTL_SLOT`                | 枠の番号                        |
| `SLOTCTL_NAME`                | 枠の名前（`<project>-<pool>-<番号>`） |
| `SLOTCTL_HOLDER`              | holderのpath                    |
| `SLOTCTL_PORT_<名前の大文字>` | そのpoolのport。例：`SLOTCTL_PORT_WEB` |
| machineの設定の`env`          | `[projects.<name>] env`の各項目 |

`down`には、止める枠のholderの値が渡る。

## 終了code

| code | 意味                                                                        |
| ---- | --------------------------------------------------------------------------- |
| 0    | 成功                                                                        |
| 1    | その他の失敗（`up`の失敗、`release`・`reclaim`の`down`の失敗、設定・DBの誤りなど）     |
| 2    | 使い方の誤り（未知の命令・flag・pool、pool未指定の`acquire`、`acquire`・`status`で`slotctl.toml`が無い） |
| 3    | poolに空きが無い（`acquire`）                                               |

## 記録

`$XDG_STATE_HOME/slotctl/events-YYYY-MM.jsonl`にJSON Linesで追記する。

```json
{"at":"2026-09-29T14:00:00+09:00","event":"down","project":"myapp","pool":"dev","slot":1,"holder":"/work/a","ok":true,"ms":820}
{"at":"2026-09-29T14:00:00+09:00","event":"reclaim","project":"myapp","pool":"dev","slot":1,"holder":"/work/a","trigger":"acquire","ok":true,"ms":825}
{"at":"2026-09-29T14:00:01+09:00","event":"acquire","project":"myapp","pool":"dev","slot":1,"holder":"/work/b","ok":true,"ms":830}
```

| 項目 | 意味 |
| -- | -- |
| `at` | 時刻（RFC 3339） |
| `event` | `acquire`・`release`・`reclaim`・`up`・`down`。`renew`は書かない |
| `project`・`pool`・`slot` | 対象の枠。空きが無くて借りられなかったときは`slot`が無い |
| `holder` | 対象の借り手。`reclaim`では回収する枠の元のholder |
| `trigger` | `reclaim`で、きっかけの命令（`acquire`または`reclaim`） |
| `ok`・`ms`・`error` | 成否・所要ミリ秒・失敗の内容 |

`acquire`は新しく借りたときと空きが無かったときに書く。回収の失敗も`reclaim`に書く。書くときに、今月を含む直近`log_retention_months`か月より古い記録を消す。

## やらないこと

- **processの管理**（起動・監視・再起動）。枠の中身は`up`・`down`が作り、slotctlは走らせるだけである。
- **期限の無い保持**。全てのleaseに期限がある。
- **資源ごとの知識**（ngrok・emulator・Dockerなど）。資源の扱いは、poolの`up`・`down`に書く。
- **複数のpoolの枠を、1つのtransactionでまとめて借りること**。まとめて握る物は、1つのpoolにする。
- 待ち行列、worktree消滅時の回収、reset命令、launchd、サブエージェントの判定、machine全体の上限。

## hookの設定例

PreToolUseでは`renew`、Stopでは自分の`renew`に続けて全poolの`reclaim`、SessionEndでは`release`を呼ぶ。hookのwrapperは失敗しても終了code 0で終わり、`reclaim`で止められなかった枠の知らせだけをsessionへ出す。

### Claude Code（`.claude/settings.json`）

```json
{
  "hooks": {
    "Stop": [{ "hooks": [{ "type": "command", "command": "bin/slotctl-hook stop" }] }],
    "PreToolUse": [{ "hooks": [{ "type": "command", "command": "bin/slotctl-hook renew" }] }],
    "SessionEnd": [{ "hooks": [{ "type": "command", "command": "bin/slotctl-hook release" }] }]
  }
}
```

`bin/slotctl-hook`はprojectが用意するwrapperである。`stop`では`slotctl renew`の次に`slotctl reclaim`を呼び、`renew`では`slotctl renew`だけ、`release`では`slotctl release`を呼ぶ。

### Codex

Codexでも同じeventと命令の対応にする。hookの書式は各ツールの文書に従う。`renew`・`release`・`reclaim`は`slotctl.toml`の無い場所で何もせず0で終わる。

`acquire <pool>`は、session自身またはproject側のscriptが呼ぶ。枠の中身の扱いは`[pools.<名前>]`のcommandが決める。

## 開発

```sh
mise exec -- go vet ./...
mise exec -- go test ./...
```

```text
cmd/slotctl/        入口（薄い）。同時申請のtestはここでbinaryをbuildして行う
internal/config/    slotctl.tomlの探索、pool、machine設定、SLOTCTL_HOMEとXDGの解決
internal/store/     SQLite（schema・移行・transaction・projects・port帯・leases）
internal/lease/     acquire・renew・release・reclaim・statusの規則（時刻は注入）
internal/command/   up・downをenvつきで`sh -c`で走らせる
internal/eventlog/  JSON Linesの追記、月ごとのfile、ローテーション
internal/holder/    holderとrepositoryの識別
internal/ports/     portの帯と割り当て
internal/cli/       flag・出力・終了code
```

## ライセンス

[MIT License](LICENSE)。Copyright (c) 2026 taknya
