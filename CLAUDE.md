# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 빌드 / 설치 / 실행

- 빌드만: `./install.sh build` (산출물: `./mdv`)
- 시스템 설치: `./install.sh` (`/usr/local/bin/mdv`, 필요 시 sudo)
- 제거: `./install.sh uninstall`
- 직접 빌드: `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o mdv .`
- 의존성 정리: `go mod tidy`
- 실행: `./mdv path/to/file.md` 또는 `./mdv --port 9000 path/to/file.md`
  - 지정 포트가 사용 중이면 자동으로 임의의 빈 포트로 폴백한다.
- 테스트 / 린트 / 포맷터 설정은 현재 리포에 없다. `go vet ./...`, `gofmt -l .` 정도가 표준 점검 명령이다.

## 아키텍처 개요

단일 파일 Go 서버(`main.go`)로 구성된 라이브 리로드 마크다운 뷰어다. 다음 흐름을 이해해야 여러 파일을 가로지르는 작업이 가능하다.

- **렌더 파이프라인**: `goldmark`(GFM, Footnote, DefinitionList, Typographer, goldmark-highlighting + chroma) → `renderMarkdown()` 가 HTML 문자열을 `template.HTML`로 반환 → `templates/view.html`에 주입.
- **테마별 코드 하이라이팅**: `init()` 에서 chroma의 `github` / `github-dark` 스타일을 각각 `html[data-theme="light"] { ... }` / `html[data-theme="dark"] { ... }` 블록으로 감싸 한 번에 생성한 CSS 문자열(`chromaCSS`)을 템플릿에 인라인한다. 테마 전환은 `<html data-theme>` 속성만 바꾸면 즉시 반영된다(브라우저의 CSS nesting 사용).
- **정적 자산 / 템플릿**: `assets/style.css` 와 `templates/view.html` 은 `embed.FS` / 문자열 임베드로 바이너리에 포함된다. 새 정적 파일을 추가하면 `//go:embed` 지시문도 함께 업데이트해야 한다.
- **라우팅 (Gin, ReleaseMode)**:
  - `GET /` — `?file=` 쿼리로 같은 디렉터리 내 다른 .md/.markdown 파일을 전환. `resolveTarget()` 이 절대경로/`..` 이스케이프/비-마크다운/디렉터리를 모두 거부한다 (디렉터리 트래버설 방지). 현재 보고 있는 파일은 `currentFile` (RWMutex) 에 갱신된다.
  - `GET /events` — SSE 스트림. `broadcaster` 에 구독하고, 25초 ping 으로 keep-alive, 클라이언트 컨텍스트 취소 시 종료.
  - `GET /assets/*` — 임베드된 정적 파일.
- **파일 감시 → 리로드**: `watchDir()` 가 `baseDir`(= 시작 파일의 디렉터리)에 `fsnotify` 워처를 건다. Write/Create/Rename 이벤트만 추리고, 이벤트 경로가 `current.get()` 과 일치할 때만 **80ms 디바운스** 후 `broadcaster.publish()` 호출 → SSE 로 `event: reload` 전송 → 브라우저가 `location.reload()`. 즉, 디렉터리 내 다른 파일을 편집해도 현재 열린 파일이 아니면 리로드하지 않는다.
- **보안 경계**: 서빙 가능한 마크다운 파일은 `baseDir` 하위만이며 `resolveTarget` 이 게이트키퍼다. 새 라우트로 파일을 노출할 때는 반드시 이 함수를 거치게 한다. 한편 goldmark는 `html.WithUnsafe()` 로 동작하므로 마크다운 원문의 raw HTML이 그대로 렌더된다 — 신뢰할 수 없는 입력을 다루도록 확장하지 말 것.
- **네트워크**: `pickListener()` 가 `0.0.0.0:<preferred>` 우선, 실패 시 임의 포트. `outboundIP()` 는 8.8.8.8 로 UDP dial 해 NIC IP를 추정(연결은 만들지 않음). 시작 시 LAN 접속용 URL과 localhost URL 두 가지를 출력한다.

## 프런트엔드 (templates/view.html)

- FOUC 방지를 위해 `<head>` 안의 인라인 스크립트가 `localStorage('mdv-theme')` 또는 `prefers-color-scheme` 으로 `data-theme` 을 즉시 설정한다.
- 사이드바는 같은 디렉터리의 .md 파일 목록을 렌더(서버측 `listMarkdownFiles`, 대소문자 무시 정렬). 현재 파일에 `.active` 클래스가 붙는다.
- 하단 스크립트가 테마 토글, 사이드바 열기/닫기(Escape/Scrim 클릭 포함), `EventSource('/events')` 리로드 구독을 담당한다. 브라우저가 자동 재연결하므로 별도 백오프 코드는 없다.
