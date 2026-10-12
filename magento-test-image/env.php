<?php
// Canonical envvars-mode env.php per docs/image-contract.md.
// Reads every configuration value from environment variables the
// mage-operator mounts into the pod via envFrom. Do not hardcode
// credentials here. Do not decode MAGENTO_CRYPT_KEY — the operator
// emits it in a format Magento reads verbatim (ADR 003).

// remote_storage is configured only when the operator emits
// MAGENTO_MEDIA_STORAGE_BUCKET (= spec.media.s3 mode). For unset
// and PVC modes the operator emits no MAGENTO_MEDIA_STORAGE_* keys,
// and we fall back to the local-disk file driver — matching what
// the mounted volume (emptyDir or PVC) provides at /app/pub/media.
$remoteStorage = ['driver' => 'file'];
if ($mediaBucket = getenv('MAGENTO_MEDIA_STORAGE_BUCKET')) {
    $remoteStorage = [
        'driver' => 'aws-s3',
        'config' => [
            'bucket'      => $mediaBucket,
            'region'      => getenv('MAGENTO_MEDIA_STORAGE_REGION') ?: '',
            'credentials' => [
                'key'    => getenv('MAGENTO_MEDIA_STORAGE_ACCESS_KEY_ID') ?: '',
                'secret' => getenv('MAGENTO_MEDIA_STORAGE_SECRET_ACCESS_KEY') ?: '',
            ],
        ],
    ];
    // Endpoint is only set for non-AWS S3-compatible providers
    // (Scaleway, Minio, Wasabi, R2). AWS S3 leaves it unset so the
    // SDK picks the regional endpoint from `region`.
    if ($mediaEndpoint = getenv('MAGENTO_MEDIA_STORAGE_ENDPOINT')) {
        $remoteStorage['config']['endpoint'] = $mediaEndpoint;
    }
}

// Redis cache: wire default + page_cache frontends when MAGENTO_REDIS_HOST is set.
// Absent = Magento falls back to the database cache (Zend_Cache_Backend_File in
// developer mode, db_ddl in production) — usable but slow.
//
// Sentinel HA mode (mage-operator v0.3.0+ inline cache with replicas >= 3):
// MAGENTO_REDIS_SENTINEL_MASTER carries the Sentinel monitor name. When
// present, MAGENTO_REDIS_HOST points at the Sentinel ClusterIP Service and
// MAGENTO_REDIS_PORT is 26379 (Sentinel protocol port). Cm_Cache_Backend_Redis
// reads `server` as a comma-separated list of sentinel host:port pairs and
// uses `sentinel_master` to select the monitored master, then queries
// Sentinel for the current master's address.
$cacheConfig = [];
if ($redisHost = getenv('MAGENTO_REDIS_HOST')) {
    $redisPort = getenv('MAGENTO_REDIS_PORT') ?: '6379';
    $redisCfg = [
        'server'        => $redisHost,
        'port'          => $redisPort,
        'password'      => getenv('MAGENTO_REDIS_PASSWORD') ?: '',
        'compress_data' => '1',
    ];
    if ($sentinelMaster = getenv('MAGENTO_REDIS_SENTINEL_MASTER')) {
        // Sentinel-discovery mode: single Sentinel ClusterIP service is
        // sufficient because kube-dns load-balances the underlying pods
        // and Sentinel's protocol handles peer discovery itself.
        $redisCfg['server'] = $redisHost . ':' . $redisPort;
        unset($redisCfg['port']);
        $redisCfg['sentinel_master'] = $sentinelMaster;
    }
    $cacheConfig['frontend']['default'] = [
        'id_prefix'       => 'mg_',
        'backend'         => 'Magento\\Framework\\Cache\\Backend\\Redis',
        'backend_options' => $redisCfg,
    ];
    $cacheConfig['frontend']['page_cache'] = [
        'id_prefix'       => 'mg_pc_',
        'backend'         => 'Magento\\Framework\\Cache\\Backend\\Redis',
        'backend_options' => array_merge($redisCfg, ['database' => 1]),
    ];
}

// consumers_wait_for_messages = 1 makes each consumer process block on
// the queue waiting for new messages instead of exiting when the queue
// drains. Required when consumers run as long-lived Deployments — the
// shape the operator deploys today (one Deployment per consumer name,
// replicas: 1, restartPolicy implicit Always). Set to 0 only if
// migrating to cron-managed consumers, where fast-exit on drain is the
// desired behaviour.
$queueConfig = ['consumers_wait_for_messages' => 1];

// RabbitMQ: wire amqp connection when MAGENTO_RABBITMQ_HOST is set.
// Absent = Magento queue consumers fall back to synchronous in-process execution.
if ($rabbitHost = getenv('MAGENTO_RABBITMQ_HOST')) {
    $queueConfig['amqp'] = [
        'host'        => $rabbitHost,
        'port'        => getenv('MAGENTO_RABBITMQ_PORT') ?: '5672',
        'user'        => getenv('MAGENTO_RABBITMQ_USERNAME') ?: '',
        'password'    => getenv('MAGENTO_RABBITMQ_PASSWORD') ?: '',
        'virtualhost' => getenv('MAGENTO_RABBITMQ_VHOST') ?: '/',
        'ssl'         => '',
    ];
}

return [
    'backend' => ['frontName' => 'admin'],
    'crypt' => ['key' => getenv('MAGENTO_CRYPT_KEY')],
    'db' => [
        'connection' => [
            'default' => [
                'host'           => getenv('MAGENTO_DATABASE_HOST') . ':' . getenv('MAGENTO_DATABASE_PORT'),
                'dbname'         => getenv('MAGENTO_DATABASE_NAME'),
                'username'       => getenv('MAGENTO_DATABASE_USERNAME'),
                'password'       => getenv('MAGENTO_DATABASE_PASSWORD'),
                'model'          => 'mysql4',
                'engine'         => 'innodb',
                'initStatements' => 'SET NAMES utf8;',
                'active'         => '1',
            ],
        ],
        'table_prefix' => '',
    ],
    'resource'    => ['default_setup' => ['connection' => 'default']],
    'MAGE_MODE'   => getenv('MAGE_MODE') ?: 'production',
    'install'     => ['date' => getenv('MAGENTO_INSTALL_DATE') ?: 'Sun, 01 Jan 2023 00:00:00 +0000'],
    'directories' => ['document_root_is_pub' => true],
    'session'     => ['save' => 'files'],
    'lock'        => ['provider' => 'db'],
    'cache_types' => [
        'config'                  => 1,
        'layout'                  => 1,
        'block_html'              => 1,
        'collections'             => 1,
        'reflection'              => 1,
        'db_ddl'                  => 1,
        'compiled_config'         => 1,
        'eav'                     => 1,
        'customer_notification'   => 1,
        'config_integration'      => 1,
        'config_integration_api'  => 1,
        'full_page'               => 1,
        'config_webservice'       => 1,
        'translate'               => 1,
    ],
    'cache'          => $cacheConfig,
    'queue'          => $queueConfig,
    'remote_storage' => $remoteStorage,
];
