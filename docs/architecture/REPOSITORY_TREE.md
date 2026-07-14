# Repository tree

```text
veqri/
├── apps/
│   ├── android/                 native Kotlin/Compose client
│   └── desktop/                 React/TypeScript companion
├── cmd/
│   ├── veqri-core/              daemon
│   └── veqri-cli/               authenticated CLI
├── core/
│   ├── agents/ approvals/ conversation/ delivery/ events/
│   ├── observability/ persistence/ policy/ tasks/ tools/ voice/
├── connectors/
│   ├── slack/ mattermost/ teams/ webhook/ local_events/
├── agents/
│   ├── general/ coding/ research/ automation/ mock/
├── tools/
│   ├── shell/ filesystem/ git/ http/ native_apps/ notifications/
├── protocol/
│   ├── proto/veqri/v1/
│   └── generated/{android,go}/
├── deploy/
│   ├── docker/ systemd/ launchd/ windows/
├── docs/
│   ├── architecture/ adr/
│   └── product and operational guides
├── scripts/
├── tests/
│   ├── integration/ e2e/ fixtures/
├── .github/workflows/ci.yml
├── .env.example
├── Makefile
├── go.mod
└── README.md
```

Empty extension directories are retained only where the architecture explicitly reserves an adapter family. Operational status is stated in the root README; an empty directory is never counted as completed functionality.
