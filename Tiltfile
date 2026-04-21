allow_k8s_contexts('k3d-audit-pass-through-e2e')

update_settings(
    max_parallel_updates=1,
    k8s_upsert_timeout_secs=180,
)

local_resource(
    'e2e-prepare',
    cmd='task e2e:prepare',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=True,
    labels=['setup'],
)

local_resource(
    'proxy-update',
    cmd='task --force e2e:build-image && task --force e2e:load-image && task --force e2e:deploy-proxy',
    deps=[
        'cmd',
        'pkg',
        'go.mod',
        'go.sum',
        'Dockerfile',
        'charts',
    ],
    trigger_mode=TRIGGER_MODE_AUTO,
    auto_init=False,
    resource_deps=['e2e-prepare'],
    labels=['proxy'],
)

local_resource(
    'mock-webhook-update',
    cmd='task --force e2e:build-mock-webhook-image && task --force e2e:load-mock-webhook-image && task --force e2e:deploy-mock-webhook',
    deps=[
        'cmd/mock-audit-webhook',
        'go.mod',
        'go.sum',
        'Dockerfile',
    ],
    trigger_mode=TRIGGER_MODE_AUTO,
    auto_init=False,
    resource_deps=['e2e-prepare'],
    labels=['supporting'],
)

local_resource(
    'smoke-test',
    cmd='task e2e:test-smoke',
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    resource_deps=['proxy-update', 'mock-webhook-update'],
    labels=['tests'],
)
