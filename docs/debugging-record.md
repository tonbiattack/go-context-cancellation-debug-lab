# 調査記録：HTTP要求の終了後に重い処理が残る

## 対象と期待値

対象は `TestClientCancellationStopsDownstreamWork` です。HTTPクライアントが要求をキャンセルし、サーバーの `request.Context().Done()` が閉じられたなら、下位のDB相当処理も同じ要求由来の `Context` を受けて `ctx.Done()` を観測し、goroutineを終了することを期待します。

| 観測点 | 期待値 | 修正前の実測値 |
| --- | --- | --- |
| クライアントの `Do` | `context.Canceled` で戻る | `context.Canceled` で戻った |
| `request.Context().Done()` | キャンセル後に閉じる | `context canceled` を観測した |
| 下位処理の `ctx.Done()` | キャンセル後100ms以内に閉じる | 閉じなかった |
| 下位処理goroutine | `ctx.Err()` を返して終了する | 250ms待って通常完了した |

## 実行環境

| 項目 | 値 |
| --- | --- |
| Go | `go version go1.22.2 linux/amd64` |
| HTTPサーバー | `net/http/httptest` |
| 疑似DB待機時間 | 250ms |
| 下位キャンセル待機の上限 | 100ms |
| 再現コミット | `3f2bf72` |

## 再現コマンドと実測ログ

```bash
git checkout 3f2bf72
go test -count=1 -run '^TestClientCancellationStopsDownstreamWork$' -v ./...
```

```text
handler: start request_ctx_err=<nil>
store: start ctx_err=<nil>
handler: request ctx.Done request_ctx_err=context canceled
lab_test.go:56: 下位処理のctx.Done() を 100ms 以内に観測できませんでした
store: completed ctx_err=<nil>
```

HTTPクライアントのキャンセルにより、サーバーが持つ `request.Context()` は終了しました。しかし、`store` が受け取ったContextは終了せず、`ctx.Err()` も最後まで `nil` でした。したがって、HTTP要求は終了している一方、重い処理のgoroutineは250ms後まで残りました。

## 仮説と検証

| 仮説 | 検証 | 結果 |
| --- | --- | --- |
| クライアントのキャンセルがHTTPサーバーへ届いていない | ハンドラ内で `request.Context().Done()` と `requestCtx.Err()` を記録 | 棄却。`context canceled` を観測した |
| `SlowStore` が `ctx.Done()` を待っていない | `SlowStore.Load` の `select` を確認 | 棄却。`case <-ctx.Done()` がある |
| ハンドラが要求由来のContextを下位へ渡していない | `SlowStore.Load` の引数を確認 | 採用。`context.Background()` が渡されていた |

## 原因

`ServeHTTP` が `r.Context()` を `requestCtx` として取得しているにもかかわらず、下位処理の呼び出し時に `context.Background()` を新しく作っていました。`Background()` は要求の子Contextではないため、クライアント切断、要求キャンセル、要求由来のタイムアウトという取消連鎖を持ちません。下位処理の `select` 自体は正しくても、監視しているContextが誤っているため処理を止められません。

## 修正方針

ハンドラで `r.Context()` から処理用のContextを派生させ、下位のDB/API相当処理まで同じContextを引数として渡します。時間上限が必要な場合は `context.WithTimeout(r.Context(), timeout)` を用い、`defer cancel()` で資源を解放します。下位処理は `select { case <-ctx.Done(): return ctx.Err() }` を維持します。

## 修正後の実測

`context.Background()` を渡していた箇所を、`r.Context()` または `context.WithTimeout(r.Context(), timeout)` から作った `workCtx` に置き換えました。`go test -count=1 -v ./...` と `go test -count=1 -race ./...` を実行し、いずれも成功しました。

| シナリオ | 下位処理の `ctx.Err()` | HTTP側の結果 | goroutine |
| --- | --- | --- | --- |
| クライアントが要求をキャンセル | `context canceled` | クライアントは `context.Canceled` | `WorkerExited()` を100ms以内に観測 |
| 40msの処理用タイムアウト | `context deadline exceeded` | `504 Gateway Timeout` | `WorkerExited()` を100ms以内に観測 |

```text
# クライアントキャンセル時
handler: start request_ctx_err=<nil>
store: start ctx_err=<nil>
store: ctx.Done ctx_err=context canceled
handler: request ctx.Done request_ctx_err=context canceled

# 処理用タイムアウト時
handler: start request_ctx_err=<nil>
store: start ctx_err=<nil>
store: ctx.Done ctx_err=context deadline exceeded
handler: request ctx.Done request_ctx_err=context canceled
```

タイムアウト時にハンドラが返った後、`net/http` が要求Contextを終了するため、ハンドラ側の要求Contextの監視ログは `context canceled` になります。一方、下位処理が受け取った `workCtx` は `context.WithTimeout` で派生したContextなので、停止原因は `context deadline exceeded` として観測されます。この二つを混同しないことが、ログからキャンセル経路を切り分けるポイントです。

## 回帰確認

```bash
gofmt -w lab.go lab_test.go
go test -count=1 -v ./...
go test -count=1 -race ./...
```

修正・文書化コミットは、すべてのテストが通る状態で作成します。
