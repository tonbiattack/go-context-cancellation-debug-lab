# go-context-cancellation-debug-lab

`net/http` のHTTPリクエストがクライアント都合でキャンセルされた後も、サーバー内の重い処理が動き続ける不具合を、失敗テストから再現・観測・修正するGoのデバッグ学習用サンプルです。

この題材では、HTTPハンドラは `request.Context()` のキャンセルを正しく検出している一方、下位のgoroutineには `context.Background()` を渡していたため、処理が停止しない状態を扱います。修正後は `request.Context()` をそのまま下位処理へ伝播し、`select` と `ctx.Done()` で終了させます。

## 対象とする不具合

```text
クライアント
  │ POST /reports
  ▼
HTTPハンドラ
  │ request.Context() はキャンセル済み
  ├── goroutine ── context.Background() を渡す（不具合）
  ▼
重い処理（DB/API呼び出し相当）
  └── HTTPリクエスト終了後も継続する
```

| 観測対象 | 修正前 | 修正後 |
| --- | --- | --- |
| HTTPリクエスト側の `ctx.Done()` | クライアント切断・タイムアウト時に閉じる | 同じ |
| HTTPリクエスト側の `ctx.Err()` | `context.Canceled` または `context.DeadlineExceeded` | 同じ |
| 下位処理に渡るcontext | `context.Background()`、`Done()` は `nil` | `request.Context()`、`Done()` は有効 |
| 下位goroutine | 100 ms以内に終了しない | `ctx.Done()` を受信して終了する |
| DB/API相当の待機 | 処理時間まで継続する | キャンセル時点で中断する |

## リポジトリ構成

| パス | 役割 |
| --- | --- |
| `cancellation.go` | HTTPハンドラと重い下位処理の最小実装 |
| `cancellation_test.go` | クライアントキャンセルとタイムアウトの再現・回帰テスト |
| `observation.go` | `ctx.Done()`、`ctx.Err()`、goroutine数を記録する観測用コード |
| `docs/investigation.md` | 実行済みの失敗ログ、原因、修正、検証記録 |

## 実行要件

Go 1.23以降を想定しています。外部依存はありません。

```bash
go version
go test -v -race ./... -count=1
```

## 不具合を再現する

修正前コミットでは、期待する「キャンセル後に下位goroutineが終了する」というテストが失敗します。`-v` を付けることで、HTTP側と下位処理側のcontext状態を比較できます。

```bash
git checkout 31b627b
go test -v ./... -count=1
```

失敗時の重要な観測結果は次のとおりです。

```text
http: request context Done observed         ctx.Err=context canceled           Doneがnil=false
worker: launched with context.Background    ctx.Err=<nil>                      Doneがnil=true
worker: simulated DB call started           ctx.Err=<nil>                      Doneがnil=true
```

HTTPリクエストのcontextはキャンセル済みです。しかし `context.Background()` はキャンセルされず、deadlineも持たないため、下位処理は `ctx.Done()` を受信できません。

## 原因

原因は、リクエストの寿命に属する処理へ `context.Background()` を渡して、親となる `request.Context()` との関係を切断していたことです。加えて、重い処理が待機時間だけを待ち、`ctx.Done()` を同時に監視していませんでした。

`context.Background()` はアプリケーションの起動、初期化、テストなどで使う空のルートcontextであり、HTTPリクエストに付随する処理の代替ではありません。[Goのcontextパッケージの公式ドキュメント](https://pkg.go.dev/context)は、入力リクエストのcontextを関数呼び出しの連鎖へ伝播するよう説明しています。

## 修正

修正は二点です。第一に、ハンドラで取得した `request.Context()` を下位処理に渡します。第二に、下位処理が実際の待機対象と `ctx.Done()` を同じ `select` で待ちます。

```go
requestCtx := r.Context()
go func() {
    _ = h.Worker.Run(requestCtx)
}()
```

```go
select {
case <-ctx.Done():
    return ctx.Err()
case <-timer.C:
    return nil
}
```

実運用では、DBには `QueryContext`、`ExecContext`、`BeginTx` などのcontext対応APIを選び、外部HTTP呼び出しには `http.NewRequestWithContext` を使います。外部呼び出しがcontextを受け取らない場合は、呼び出し境界で中断できる手段を別途設計してください。

## 回帰テスト

修正済みの `main` では、同じテストが次の契約を検証します。

| テスト | 入力 | 検証する契約 |
| --- | --- | --- |
| `TestClientCancellationStopsWorker` | 呼び出し元が明示的に `cancel()` | ハンドラと下位goroutineが終了し、`context.Canceled` を返す |
| `TestTimeoutStopsWorker` | 30 msの期限付きcontext | ハンドラと下位goroutineが終了し、`context.DeadlineExceeded` を返す |

```bash
git checkout main
go test -v -race ./... -count=1
```

修正後のログでは、下位処理側も `Doneがnil=false` になり、キャンセル後に `worker: ctx.Done observed; goroutine exits` が記録されます。詳細なログ、仮説、Gitコミットは [調査記録](docs/investigation.md) を参照してください。

## このサンプルの範囲

このサンプルは、キャンセルを「通知する」だけでなく、下位処理が通知を実際に受け取って終了するまでを小さく観測するための教材です。実際のHTTPクライアントのTCP切断そのもの、DBドライバ固有のキャンセル保証、サーバーのgraceful shutdownは扱いません。それらには個別の接続・ドライバ・運用設定の検証が必要です。

## 参考資料

- [context package - Go Packages](https://pkg.go.dev/context)
- [http.Request.Context - Go Packages](https://pkg.go.dev/net/http#Request.Context)
- [http.NewRequestWithContext - Go Packages](https://pkg.go.dev/net/http#NewRequestWithContext)
