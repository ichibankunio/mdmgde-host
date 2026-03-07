# シーン遷移の現状メモ

Issue #5 向けに、現在の実装だけをコードベースで整理したメモです。

## 定義されているシーン

- `SCENE_TITLE` (`0`)
- `SCENE_EDIT` (`1`)

定義と登録:

- `game/main.go` の `const` で定義
- `Game.Init()` で `TitleScene` と `EditScene` を `AddScene`

## 起動時にどのシーンから始まるか

起動エントリによって `FlibGame.State` が異なります。

- WASM (`make wasm` で使う `./cmd/game`): `State: 0` なので `TitleScene` 開始
- ローカル実行 (`go run .` の `main.go`): `State: 1` なので `EditScene` 開始
- mobile (`mobile/mobile.go`): 初期値は `State: 0` だが、ネイティブ側から `LoadState` で任意シーンへ移動可能

## 実際にコード上で発生する遷移

### 1. Title -> Edit

`TitleScene.Update()` で `1` キー押下時に以下を実行:

- `flib.ShiftSceneWithFadeInOut(g, SCENE_EDIT, 2)`

### 2. (WASM専用関数内) Title相当 -> Edit

`game/wasm.go` の `wasmIO()` では、画像選択後に以下を実行:

- `flib.ShiftSceneWithFadeInOut(g, SCENE_EDIT, 2)`

ただし現状は `TitleScene.Init()` 内の `wasmIO(g)` 呼び出しがコメントアウトされているため、この経路は通常の実行では使われません。

## 現状ない遷移

- `EditScene` から `TitleScene` へ戻る処理は未実装
- `EditScene` から他シーンへ進む処理も未実装
- `Escape`/ゲームパッド `Button8` はシーン遷移ではなく `fmt.Errorf("exit")` を返して終了扱い

## 全体フロー（現状）

`TitleScene` 開始時:

`TitleScene` --(Key1)--> `EditScene`

`EditScene` 開始時:

`EditScene` のみ（他シーン遷移なし）
