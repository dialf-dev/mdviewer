# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 빌드 / 설치 / 실행

- 빌드만: `./install.sh build` (산출물: `./mdv`)
- 서비스 설치: `./install.sh` — 바이너리(`/usr/local/bin/mdv`) + systemd 유닛(`/etc/systemd/system/mdv.service`, root) 설치 후 `enable --now`. 제거: `./install.sh uninstall`
- 직접 빌드: `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o mdv .`
- 테스트 / 린트 설정은 없다. `go vet ./...`, `gofmt -l .` 이 표준 점검 명령.
- 루트 없이 로컬에서 서버를 돌려볼 때는 env 오버라이드를 쓴다:
  `MDV_CONFIG=/tmp/t/config.json MDV_SOCKET=/tmp/t/mdv.sock ./mdv serve --port 19000`
  (CLI 명령에도 같은 env를 줘야 같은 인스턴스에 붙는다. 업로드 디렉토리는 config의 `upload_dir`로 지정.)

## CLI 명령 체계

단일 바이너리가 서버와 클라이언트를 겸한다 (`main.go` 디스패치).

- `mdv serve [--port N]` — 서버 (systemd가 실행; `--port`는 비영속 오버라이드)
- `mdv on` / `mdv off` — 외부 접속(0.0.0.0) 토글. 서버가 listener를 in-process 재바인드
- `mdv add [dir]` / `mdv del [dir]` — 디렉토리 등록/해제 (기본: cwd, 절대경로로 저장)
- `mdv list` / `mdv status` — 등록 목록 / 바인드·포트·URL 상태
- `mdv <file.md>` — 실행 중인 서비스에 temp 문서로 등록하고 URL 출력 후 즉시 종료. 해당 파일의 디렉토리가 사이드바에 "temp" 뱃지로 표시되고 탐색 가능. temp는 config에 저장되지 않아 서비스 재시작 시 소멸. 첫 화면(`/`)은 마지막 temp 문서로 redirect

## 아키텍처 개요

상시 실행 문서 뷰어 서버. 파일 구성: `main.go`(CLI), `server.go`(HTTP/제어소켓/루트관리), `tree.go`(사이드바 트리), `hub.go`(SSE+감시), `markdown.go`(렌더), `config.go`(설정).

- **설정 단일 소스**: `/etc/mdv/config.json` (env `MDV_CONFIG` 오버라이드). port(기본 9000), external, secret(HMAC 키), roots(절대경로 목록), upload_dir(`/var/lib/mdv/upload`). 서버만 이 파일을 쓴다 — CLI는 항상 unix socket(`/run/mdv.sock`, env `MDV_SOCKET`)을 통해 서버에 요청하고 서버가 config를 갱신한다. 소켓은 0666 (무인증 설계).
- **불투명 문서 URL**: 모든 문서는 `/d/<docID>` 로 서빙. docID = `hex(HMAC-SHA256(secret, 절대경로))[:16]`. 단방향이므로 서버가 메모리 맵(docID→경로)을 유지하며, 시작 시·add 시·temp 시 디렉토리 walk(`indexRoot`)와 사이드바 렌더 시점(`rememberDoc`)에 채워진다. 경로가 URL에 노출되지 않고, secret이 영속이라 재시작 후에도 링크가 유지된다.
- **접근 제어 경계**: `isAllowedFile()` 이 게이트키퍼 — 요청 시점마다 "등록 루트(+upload_dir, temp 루트) 하위의 마크다운 파일"인지 검증한다. del 직후 stale 맵 엔트리가 남아도 이 검사로 404가 된다. 새 라우트로 파일을 노출할 때 반드시 이 함수를 거칠 것.
- **루트 병합**: 부모/자식 디렉토리가 동시에 등록되면 사이드바에는 부모만 보인다(`mergeRoots`). config의 roots 목록 자체는 건드리지 않으므로 부모를 del하면 자식들이 다시 개별 표시된다. temp 루트도 같은 규칙 + 등록 루트에 덮이면 숨김.
- **외부 접속 토글**: `setExternal()` 이 config 저장 후 `httpSrv.Close()` → `run()` 루프가 새 바인드 주소(127.0.0.1 ↔ 0.0.0.0)로 재리슨. 포트 사용 중이면 임의 포트 폴백(`pickListener`).
- **라이브 리로드 (lazy watching)**: `hub` 가 SSE 구독(`/events?d=<docID>`)이 있는 문서의 디렉토리만 fsnotify로 감시한다(디렉토리별 refcount, 구독 해제 시 watch 제거). 등록 트리 전체를 감시하지 않으므로 inotify watch 한도 문제가 없다. 이벤트 경로의 docID를 HMAC으로 계산해 해당 구독자에게만 80ms 디바운스 후 reload 전송.
- **업로드**: `/upload` 페이지(파일 브라우저/드래그앤드롭 토글), `POST /api/upload` (multipart). .md/.markdown만, 파일당 20MB, 동명 파일은 `이름(1).md` 자동 변경(`uniquePath`, O_EXCL). upload_dir은 항상 허용 루트에 포함되어 사이드바에 노출.
- **렌더 파이프라인**: goldmark(GFM+Footnote+DefinitionList+Typographer+chroma) → `template.HTML` → `templates/view.html`. 테마별 chroma CSS는 `markdown.go:init()` 에서 `html[data-theme=...]` 블록으로 한 번 생성해 인라인. goldmark는 `html.WithUnsafe()` — raw HTML이 그대로 렌더되므로 신뢰 경계에 유의 (무인증 공개 설계상 수용된 리스크).
- **정적 자산 / 템플릿**: `assets/style.css`, `templates/view.html`, `templates/upload.html` 은 embed. 새 정적 파일 추가 시 `//go:embed` 지시문도 갱신할 것.

## 프런트엔드

- `templates/view.html`: FOUC 방지 인라인 테마 스크립트(`localStorage('mdv-theme')`), 사이드바(서버측 렌더 트리 — `{{.Tree}}`), 상단 Upload 버튼, `EventSource('/events?d=<docID>')` 구독 (docID 없으면 미구독 = 환영 화면).
- `templates/upload.html`: 모드 토글(File Browser / Drag & Drop), 업로드 큐(중복 제거·개별 삭제), `fetch POST /api/upload` 후 결과에 `/d/<id>` 링크 표시.
- 사이드바 트리는 `<details>` 기반, 현재 문서의 조상 디렉토리는 서버에서 `open` 처리, dot 파일/디렉토리와 md 없는 가지는 숨김.
