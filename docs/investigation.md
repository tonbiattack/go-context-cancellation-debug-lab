# デバッグ記録：HTTPリクエスト終了後も重い処理のgoroutineが残る

## 目的

`POST /reports` の呼び出し元がキャンセルまたはタイムアウトしたとき、HTTPリクエストに紐づくDB/API相当の重い処理も速やかに終了し、同じ取消理由を返すことを契約とします。修正前は、HTTPハンドラがリクエストの終了を検出しても、下位goroutineが `context.Background()` を起点にしているため、重い処理だけが継続しました。

## 再現条件

| 項目 | 内容 |
| --- | --- |
| バグを含むコミット | `31b627b` — `HTTPキャンセルで処理が残る不具合を再現` |
| テスト名 | `TestClientCancellationStopsWorker`、`TestTimeoutStopsWorker` |
| HTTP境界 | `httptest.NewRequest(...).WithContext(clientCtx)` で `POST /reports` を作成 |
| 操作 | 明示的な `cancel()`、または30 msの `context.WithTimeout` を発火 |
| 期待する最終状態 | ハンドラとWorkerのgoroutineが100 ms以内に終了し、Workerが取消理由を返す |

## 最初に観測した事実

| 観測対象 | 期待値 | 実際値 | 根拠 |
| --- | --- | --- | --- |
| HTTPリクエストの `Done()` | 取消時に閉じる | 閉じた | `http: request context Done observed` |
| HTTPリクエストの `Err()` | 取消理由を返す | 手動取消では `context canceled`、期限切れでは `context deadline exceeded` | テストと観測ログ |
| 下位処理に渡したcontext | 同じ取消を受け取れる | `context.Background()` のため `Done()` が `nil`、`Err()` が `nil` | `worker: launched with context.Background` |
| 下位goroutine | 100 ms以内に終了する | 完走待機中で終了しない | `Worker.Finished` の待機タイムアウト |

修正前に実行したコマンドと失敗出力です。

```bash
go test -v ./... -count=1
```

```text
=== RUN   TestClientCancellationStopsWorker
    cancellation_test.go:61: クライアントをキャンセルしたのに、重い処理のgoroutineが終了しませんでした
    cancellation_test.go:19:
        --- cancellation observation ---
        +  0ms http: handler entered                       ctx.Err=<nil>                      Doneがnil=false goroutines=3
        +  0ms worker: launched with context.Background    ctx.Err=<nil>                      Doneがnil=true  goroutines=4
        +  0ms worker: goroutine started                   ctx.Err=<nil>                      Doneがnil=true  goroutines=4
        +  0ms worker: simulated DB call started           ctx.Err=<nil>                      Doneがnil=true  goroutines=4
        +  0ms http: request context Done observed         ctx.Err=context canceled           Doneがnil=false goroutines=4
--- FAIL: TestClientCancellationStopsWorker (0.10s)

=== RUN   TestTimeoutStopsWorker
    cancellation_test.go:107: タイムアウトしたのに、重い処理のgoroutineが終了しませんでした
    cancellation_test.go:67:
        --- cancellation observation ---
        +  0ms http: handler entered                       ctx.Err=<nil>                      Doneがnil=false goroutines=4
        +  0ms worker: launched with context.Background    ctx.Err=<nil>                      Doneがnil=true  goroutines=5
        +  0ms worker: goroutine started                   ctx.Err=<nil>                      Doneがnil=true  goroutines=5
        +  0ms worker: simulated DB call started           ctx.Err=<nil>                      Doneがnil=true  goroutines=5
        + 30ms http: request context Done observed         ctx.Err=context deadline exceeded  Doneがnil=false goroutines=5
--- FAIL: TestTimeoutStopsWorker (0.13s)
```

HTTPハンドラが終了したことだけでは、起動済みの下位goroutineが終了した証拠になりません。このサンプルでは `Worker.Finished` をgoroutine終了の同期点とし、`Result` を最終的な取消理由の観測点としました。

## 仮説と切り分け

| 仮説 | 確認方法 | 結果 |
| --- | --- | --- |
| クライアント側の取消がHTTPハンドラへ届いていない | `request.Context().Done()` を待ち、`Err()` を記録する | 棄却。手動取消は `context.Canceled`、期限切れは `context.DeadlineExceeded` になった |
| ハンドラが終了していない | `handlerDone` を待つ | 棄却。両ケースでハンドラは100 ms以内に終了した |
| 下位goroutineが取消を受け取れていない | 下位処理の `Done() == nil` と `Err()` を記録する | 採用。`context.Background()` では `Done() == nil`、`Err() == nil` のままだった |
| 下位処理が取消を受け取っても停止しない | `select` の分岐と `Worker.Finished` を確認する | 採用。修正前は `time.Sleep` のみで、取消通知を待っていなかった |

## 原因

根本原因は二つです。ハンドラが `request.Context()` ではなく `context.Background()` を渡したため、リクエストのキャンセル伝播が下位処理で断ち切られていました。さらに、Workerは `time.Sleep` だけを実行し、`ctx.Done()` を待っていませんでした。

`context.Background()` はキャンセルされず、deadlineも持たない空のルートcontextです。一方、サーバーで受け取った `http.Request` のcontextは、クライアント接続の切断、リクエストのキャンセル、またはハンドラの終了時にキャンセルされます。[`context` パッケージ](https://pkg.go.dev/context)は入力リクエストから下位API境界へcontextを伝播することを説明しており、[`Request.Context`](https://pkg.go.dev/net/http#Request.Context) はサーバーリクエストにおける取消条件を定義しています。

## 修正

ハンドラから下位処理へ同一の `request.Context()` を渡し、下位処理では完了待機と `ctx.Done()` を `select` で競合させました。

```go
requestCtx := r.Context()
go func() {
    _ = h.Worker.Run(requestCtx)
}()
```

```go
select {
case <-ctx.Done():
    err := ctx.Err()
    w.Result <- err
    return err
case <-timer.C:
    w.Result <- nil
    return nil
}
```

この修正は、キャンセルを受け取った時点でWorkerのgoroutineを終了させます。下位処理がDBや外部HTTPであれば、この `ctx` を `QueryContext`、`ExecContext`、`BeginTx`、または `http.NewRequestWithContext` のようなcontext対応APIへ渡す必要があります。呼び出し境界がcontextを受け取らなければ、呼び出し元が正しく伝播しても実処理を中断できません。

## 再発防止テスト

| テスト | 守る契約 | 最終観測 |
| --- | --- | --- |
| `TestClientCancellationStopsWorker` | 明示取消をWorkerまで伝播する | `Worker.Finished` が閉じ、`Worker.Run` の結果が `context.Canceled` |
| `TestTimeoutStopsWorker` | deadline超過をWorkerまで伝播する | `Worker.Finished` が閉じ、`Worker.Run` の結果が `context.DeadlineExceeded` |

修正後に実行したコマンドと結果です。

```bash
go test -v -race ./... -count=1
```

```text
=== RUN   TestClientCancellationStopsWorker
        +  0ms worker: launched with request.Context       ctx.Err=<nil>                      Doneがnil=false goroutines=4
        +  0ms worker: ctx.Done observed; goroutine exits  ctx.Err=context canceled           Doneがnil=false goroutines=4
--- PASS: TestClientCancellationStopsWorker (0.00s)

=== RUN   TestTimeoutStopsWorker
        + 30ms http: request context Done observed         ctx.Err=context deadline exceeded  Doneがnil=false goroutines=4
        + 30ms worker: ctx.Done observed; goroutine exits  ctx.Err=context deadline exceeded  Doneがnil=false goroutines=3
--- PASS: TestTimeoutStopsWorker (0.03s)
PASS
```

`-race` 付きの全テストは `ok github.com/tonbiattack/go-context-cancellation-debug-lab 1.046s` で成功しました。

## 再現手順

```bash
# 修正前：意図した再現テストが失敗する
git checkout 31b627b
go test -v ./... -count=1

# 修正後：キャンセル・タイムアウト・競合検出を確認する
git checkout main
go test -v -race ./... -count=1
```

## 設計上の範囲

この修正は「リクエストの寿命に属する処理」に限定します。監査ログ送信、キュー投入、非同期ジョブのように、HTTPレスポンス後も意図的に続ける仕事は `request.Context()` をそのまま使うべきではありません。その場合は、継続を明示した設計、独立したタイムアウト、再試行・冪等性・観測可能性を別途実装します。安易な `context.Background()` による継続は、意図的な非同期化ではなく、取消責務を見えなくしがちです。
