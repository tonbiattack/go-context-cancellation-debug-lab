# go-context-cancellation-debug-lab

HTTPクライアントがリクエストをキャンセルした後も、サーバーの重い処理が動き続ける不具合を、Goの標準ライブラリだけで再現・観測・修正するデバッグ学習用リポジトリです。

このラボの中心は「`context` の一般的な使い方」ではありません。HTTP要求はすでに終わっているのに、なぜ下位のDB/API相当処理とそのgoroutineが残るのかを、失敗テスト、ログ、`ctx.Done()`、`ctx.Err()` で切り分けます。

## 何を再現するか

修正前のハンドラは `r.Context()` を取得しながら、下位処理に `context.Background()` を渡していました。クライアント側の要求をキャンセルするとサーバーの `request.Context()` は終了しますが、`Background()` はその子ではありません。結果として下位処理はキャンセルを受け取らず、待機時間まで動き続けます。

```text
HTTP client
  -> net/http handler
      -> SlowStore（DB/API相当の重い処理）
          -> worker goroutine

クライアントがキャンセル
  -> request.Context().Done() は閉じる
  -> context.Background() を渡したSlowStoreのctx.Done()は閉じない
  -> worker goroutineが処理完了まで残る
```

| 観測点 | 修正前 | 修正後 |
| --- | --- | --- |
| クライアントのHTTP要求 | `context.Canceled` で終了 | `context.Canceled` で終了 |
| `request.Context().Done()` | 閉じる | 閉じる |
| 下位処理の `ctx.Done()` | 閉じない | 閉じる |
| 下位処理の `ctx.Err()` | `nil` のまま通常完了 | `context.Canceled` または `context.DeadlineExceeded` |
| 下位goroutine | 疑似DB待機の250msまで残る | キャンセルまたは期限超過で終了 |

## 不具合の再現

再現用コミットは [`3f2bf72`](https://github.com/tonbiattack/go-context-cancellation-debug-lab/tree/3f2bf72) です。このコミットは意図的にテストを失敗させます。

```bash
git checkout 3f2bf72
go test -count=1 -run '^TestClientCancellationStopsDownstreamWork$' -v ./...
```

実測では、HTTP要求側のContextだけが `context canceled` になり、下位処理の `ctx.Done()` は100ms以内に閉じませんでした。その後に `store: completed ctx_err=<nil>` が出るため、要求終了後も下位処理が継続したことが分かります。完全な記録は [`docs/debugging-record.md`](docs/debugging-record.md) にあります。

## 原因

問題の行は、修正前の `ServeHTTP` にある次の呼び出しです。

```go
err := a.store.Load(context.Background(), a.events)
```

`context.Background()` は、要求の期限、クライアント切断、キャンセルを持たないルートContextです。`SlowStore.Load` は `select` で `ctx.Done()` を待っていますが、渡されたContext自体が終了しないため、キャンセル分岐へ入りません。

## 最小修正

修正後は、HTTPハンドラの `r.Context()` を下位処理へ渡します。処理に独自の時間上限が必要な場合だけ、その要求Contextから `context.WithTimeout` で子Contextを作ります。

```go
workCtx := r.Context()
cancel := func() {}
if a.workTimeout > 0 {
    workCtx, cancel = context.WithTimeout(r.Context(), a.workTimeout)
}
defer cancel()

if err := a.store.Load(workCtx, a.events); err != nil {
    // context.Canceled と context.DeadlineExceeded をHTTP応答へ変換する。
}
```

下位処理では、ブロックする待機を `select` で包みます。

```go
select {
case <-ctx.Done():
    return ctx.Err()
case <-timer.C:
    return nil
}
```

実際のDBアクセスでは疑似待機の代わりに `db.QueryContext(ctx, ...)`、`db.ExecContext(ctx, ...)` のようにContextを受け取るAPIを使います。外部HTTP呼び出しでも、`http.NewRequestWithContext(ctx, ...)` で同じContextを渡します。[Goの公式ドキュメント](https://go.dev/doc/database/cancel-operations)は、HTTP要求のContextをDB操作へ渡すと、クライアントの切断または処理用タイムアウトのどちらでも早期終了できると説明しています。

## テスト方法

Go 1.22.2で検証しています。通常の回帰テストと競合検出を次のように実行します。

```bash
go test -count=1 -v ./...
go test -count=1 -race ./...
```

| テスト | 固定する契約 | 主な観測 |
| --- | --- | --- |
| `TestClientCancellationStopsDownstreamWork` | クライアントがキャンセルしたら下位処理が止まり、worker goroutineが終了する | HTTPキャンセル、`request.Context().Done()`、下位の `ctx.Done()`、`context.Canceled` |
| `TestRequestTimeoutStopsDownstreamWork` | 要求由来の40msタイムアウトで下位処理を止め、504を返す | `context.DeadlineExceeded`、`ctx.Done()`、worker goroutine終了、通常完了しないこと |

`EventLog` は、ハンドラ開始、要求Contextの終了、下位の `ctx.Done()`、通常完了を出力します。テストが失敗した場合は、ログの順序を使って「HTTP要求が終わった地点」と「下位処理が止まった地点」を比較してください。

## Git履歴

| コミット | 意図 |
| --- | --- |
| `d1ce4a6` | `context.Background()` を誤用する最小の不具合実装を追加 |
| `3f2bf72` | クライアント切断後に下位処理が残る失敗テストを追加 |
| この後続コミット | `request.Context()` の伝播、タイムアウト、goroutine終了の回帰テストと文書化 |

## 学習上の制約

このラボは再現性を優先し、標準ライブラリの `net/http` とタイマーでDB/API相当の待機を模擬しています。本番のDBドライバや外部SDKがContextを受け取らない場合、呼び出し側だけで確実にI/Oを中断できないことがあります。その場合は、Context対応APIへの置換、ドライバ仕様の確認、接続やクエリのキャンセル方法を個別に確認してください。

また、要求の終了後も意図的に実行すべきジョブには、HTTP要求Contextをそのまま使いません。キューへ投入して受付完了を応答条件にするなど、バックグラウンド処理として独立した寿命と再試行契約を設計します。

## 参考資料

- [context package - Go Packages](https://pkg.go.dev/context)
- [Request.Context - net/http package](https://pkg.go.dev/net/http#Request.Context)
- [Canceling in-progress operations - Go Documentation](https://go.dev/doc/database/cancel-operations)
