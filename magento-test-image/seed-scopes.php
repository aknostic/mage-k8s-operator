<?php
// Seed app/etc/config.php with the default scopes (admin website + base
// website + their groups + stores). setup:install normally writes this
// at runtime; setup:static-content:deploy needs it at build time so it
// can iterate the scopes when generating per-store theme files.
//
// Values match a vanilla `bin/magento setup:install` output for an
// English store. Customers forking this image can extend with extra
// websites / store views by re-running this script with their own
// config or by letting setup:install overwrite at runtime (it merges).

$path = ($argv[1] ?? getcwd()) . '/app/etc/config.php';
if (!file_exists($path)) {
    fwrite(STDERR, "config.php not found at {$path}\n");
    exit(1);
}

$config = require $path;

$config['scopes'] = [
    'websites' => [
        'admin' => [
            'website_id' => '0', 'code' => 'admin', 'name' => 'Admin',
            'sort_order' => '0', 'default_group_id' => '0', 'is_default' => '0',
        ],
        'base' => [
            'website_id' => '1', 'code' => 'base', 'name' => 'Main Website',
            'sort_order' => '0', 'default_group_id' => '1', 'is_default' => '1',
        ],
    ],
    'groups' => [
        0 => [
            'group_id' => '0', 'website_id' => '0', 'code' => 'default',
            'name' => 'Default', 'root_category_id' => '0', 'default_store_id' => '0',
        ],
        1 => [
            'group_id' => '1', 'website_id' => '1', 'code' => 'main_website_store',
            'name' => 'Main Website Store', 'root_category_id' => '2', 'default_store_id' => '1',
        ],
    ],
    'stores' => [
        'admin' => [
            'store_id' => '0', 'code' => 'admin', 'website_id' => '0',
            'group_id' => '0', 'name' => 'Admin', 'sort_order' => '0', 'is_active' => '1',
        ],
        'default' => [
            'store_id' => '1', 'code' => 'default', 'website_id' => '1',
            'group_id' => '1', 'name' => 'Default Store View', 'sort_order' => '0', 'is_active' => '1',
        ],
    ],
];

file_put_contents($path, "<?php\nreturn " . var_export($config, true) . ";\n");
echo "scopes seeded into {$path}\n";
