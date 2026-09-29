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

```sh
go install github.com/taknya/slotctl/cmd/slotctl@latest
```

Goの版は`mise.toml`が決める。手元で試すときは次を使う。

```sh
mise install
mise exec -- go build -o slotctl ./cmd/slotctl
```

## 用語

| 語      | 意味                                                                                               |
| ------- | -------------------------------------------------------------------------------------------------- |
| project | `slotctl.toml`の`project`。枠を共有する単位。                                                      |
| 枠      | projectが持つ、番号（1から）のついた使い場所。名前は`<project>-<slot>`。                           |
| holder  | 枠の借り手。cwdのgit worktreeの実path（`git rev-parse --show-toplevel`）。gitの外ならcwdの実path。 |
| lease   | holderが枠を借りている記録。期限（`expires_at`）を持つ。                                           |

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
```

`up`・`down`は省略できる。未知のkeyはエラーにする。

### machineの設定（任意）

`$XDG_CONFIG_HOME/slotctl/config.toml`（既定`~/.config/slotctl/config.toml`）。

```toml
port_start = 12000            # projectの帯を割り当て始めるport（既定12000）
log_retention_months = 3      # 記録を残す月数（既定3）

[projects.myapp]
slots = 3                     # projectのcountを上書き
env = { MYAPP_HOME = "~/Development/myapp-local-state" }   # commandに渡すenv（先頭の~はHOMEに展開）
```

### 置き場

| 種類               | 置き場                                                      |
| ------------------ | ----------------------------------------------------------- |
| 状態（`state.db`） | `$XDG_STATE_HOME/slotctl/`（既定`~/.local/state/slotctl/`） |
| 記録               | 状態と同じdirectoryの`events-YYYY-MM.jsonl`                 |
| machineの設定      | `$XDG_CONFIG_HOME/slotctl/config.toml`                      |

環境変数`SLOTCTL_HOME`を設定すると、状態・記録・machineの設定の置き場をそのdirectory1つに差し替える（testや試用向け）。

## port

projectが初めて使われたときに、`port_start`から1000ずつ空いている帯を割り当てて記録する。portは次のとおり。

```text
port = 帯の先頭 + 100 × (slot − 1) + names内の順番
```

同じprojectの帯は変わらず、projectどうしの帯は重ならない。同じ`project`名を別のrepository（git common dir。gitの外なら`slotctl.toml`のあるdirectory）が名乗ると、命令は失敗する。

## 命令

```text
slotctl acquire [--json]   枠を借りる
slotctl renew              期限を延ばす
slotctl release            枠を返す
slotctl status [--json]    全枠の状態を出す
```

### acquire

1つのtransaction（`BEGIN IMMEDIATE`）で、次の順に枠を決める。

1. 同じholderの枠があれば、それを延長して返す（冪等。`up`は走らせない）。
2. 無ければ、空き枠（lease無し）の最も小さい番号。
3. 空きが無ければ、期限切れのうち`expires_at`が最も古い枠を譲る（前のholderを記録する）。
4. どれも無ければ、終了code 3。使っているholderと期限を出す。

transactionの外で、新しく借りた場合に`up`を走らせる。譲った場合は、先にその枠の`down`を前のholderのenvで走らせ、次に`up`を走らせる。`up`が失敗したらleaseを消して失敗を返す（前のholderの`down`の失敗は警告と記録にとどめ、`up`へ進む）。

出力は、slot・名前（`<project>-<slot>`）・port・期限。`--json`ならJSON。

```text
$ slotctl acquire
slot: 1
name: myapp-1
port: web=12000 db=12001
expires: 2026-09-29T14:10:00+09:00
```

### renew

holderのleaseの`expires_at`を「今＋ttl」にする。leaseが無ければ何もせず0で終わる。commandは走らせず、記録も書かない。hookから毎回呼ばれるので速く保つ。`slotctl.toml`が無い場所でも何もせず0で終わる。

### release

holderの枠の`down`を走らせ、leaseを消す。leaseが無ければ何もせず0で終わる。`down`が失敗してもleaseは消し、失敗を記録して終了code 1にする。`slotctl.toml`が無い場所では何もせず0で終わる。

### status

projectの全枠について、slot・名前・状態・holder・期限・portを出す。状態は次のとおり。

| 状態      | 意味                                 |
| --------- | ------------------------------------ |
| `lent`    | 貸し出し中で、期限内                 |
| `expired` | 貸し出したままで、期限切れ（譲れる） |
| `free`    | lease無し                            |

## commandに渡すenv

`up`・`down`は、holderのworktreeをcwdにして`sh -c`で走る。commandの出力はstderrへ流れる（`acquire --json`のstdoutを汚さない）。

| 名前                          | 値                              |
| ----------------------------- | ------------------------------- |
| `SLOTCTL_PROJECT`             | project名                       |
| `SLOTCTL_SLOT`                | 枠の番号                        |
| `SLOTCTL_NAME`                | `<project>-<slot>`              |
| `SLOTCTL_HOLDER`              | holderのpath                    |
| `SLOTCTL_PORT_<名前の大文字>` | 例：`SLOTCTL_PORT_WEB`          |
| machineの設定の`env`          | `[projects.<name>] env`の各項目 |

譲る前のholderの`down`には、そのholderの値が渡る。

## 終了code

| code | 意味                                                                        |
| ---- | --------------------------------------------------------------------------- |
| 0    | 成功                                                                        |
| 1    | その他の失敗（`up`の失敗、`release`の`down`の失敗、設定・DBの誤りなど）     |
| 2    | 使い方の誤り（未知の命令・flag、`acquire`・`status`で`slotctl.toml`が無い） |
| 3    | 空きが無い（`acquire`）                                                     |

## 記録

`$XDG_STATE_HOME/slotctl/events-YYYY-MM.jsonl`に、JSON Linesで追記する。

```json
{"at":"2026-09-29T14:00:00+09:00","event":"preempt","project":"myapp","slot":3,"holder":"/work/b","previous_holder":"/work/a"}
{"at":"2026-09-29T14:00:01+09:00","event":"down","project":"myapp","slot":3,"holder":"/work/a","ok":true,"ms":820}
{"at":"2026-09-29T14:00:05+09:00","event":"up","project":"myapp","slot":3,"holder":"/work/b","ok":true,"ms":4100}
{"at":"2026-09-29T14:00:05+09:00","event":"acquire","project":"myapp","slot":3,"holder":"/work/b","ok":true,"ms":4990}
```

| 項目                | 意味                                                               |
| ------------------- | ------------------------------------------------------------------ |
| `at`                | 時刻（RFC 3339）                                                   |
| `event`             | `acquire`・`preempt`・`release`・`up`・`down`。`renew`は書かない   |
| `project`・`slot`   | 対象の枠。空きが無くて借りられなかったときは`slot`が無い           |
| `holder`            | そのeventの主（`down`は、その`down`を受けるholder）                |
| `previous_holder`   | `preempt`だけ。譲られた前のholder                                  |
| `ok`・`ms`・`error` | 成否・所要ミリ秒・失敗の内容（`acquire`・`up`・`down`・`release`） |

`acquire`は、新しく借りたとき（延長ではないとき）と、空きが無くて借りられなかったときに書く。書くときに、今月を含む直近`log_retention_months`か月より古い`events-*.jsonl`を消す。

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

`acquire`は、sessionの開始時にsessionが自分で呼ぶ（またはproject側のscriptが呼ぶ）。枠の`up`・`down`が何をするかは、projectの`[commands]`が決める。

## 作らないもの

待ち行列、自然な片付け（cleanup_after）、worktree消滅時の回収、reset命令、ngrok、launchd、サブエージェントの判定、machine全体の上限。

## 開発

```sh
mise exec -- go vet ./...
mise exec -- go test ./...
```

```text
cmd/slotctl/        入口（薄い）。同時申請のtestはここでbinaryをbuildして行う
internal/config/    slotctl.tomlの探索、machine設定、SLOTCTL_HOMEとXDGの解決
internal/store/     SQLite（schema・transaction・projects・leases）
internal/lease/     acquire・renew・release・statusの規則（時刻は注入）
internal/command/   up・downをenvつきで`sh -c`で走らせる
internal/eventlog/  JSON Linesの追記、月ごとのfile、ローテーション
internal/holder/    holderとrepositoryの識別
internal/ports/     portの帯と割り当て
internal/cli/       flag・出力・終了code
```

## ライセンス

[MIT License](LICENSE)。Copyright (c) 2026 taknya
