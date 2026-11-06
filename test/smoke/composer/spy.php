<?php
// A Composer repository for the smoke test to watch: it logs what it is asked
// (docker logs) and has one advisory, of spy/vuln. Under /down/ its API fails.
$uri = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
error_log('ASKED ' . $_SERVER['REQUEST_METHOD'] . ' ' . $uri . ' ' . urldecode(file_get_contents('php://input')));
header('Content-Type: application/json');
$prefix = strpos($uri, '/down/') === 0 ? '/down' : '';
if ($uri === "$prefix/packages.json") {
    echo json_encode(['packages' => [], 'metadata-url' => "$prefix/p2/%package%.json",
        'security-advisories' => ['metadata' => true, 'api-url' => "$prefix/api/security-advisories/"]]);
} elseif ($uri === '/down/api/security-advisories/') {
    http_response_code(503);
} elseif ($uri === '/api/security-advisories/') {
    $answer = [];
    foreach ($_POST['packages'] ?? [] as $name) {
        $answer[$name] = $name !== 'spy/vuln' ? [] : [['advisoryId' => 'PKSA-spy', 'packageName' => $name, 'remoteId' => 'spy', 'title' => 'A smoke test advisory',
            'link' => 'https://example.org/spy', 'cve' => 'CVE-2026-0000', 'affectedVersions' => '<2.0', 'source' => 'spy', 'reportedAt' => '2026-01-01T00:00:00+00:00',
            'composerRepository' => 'https://example.org', 'severity' => 'high', 'sources' => []]];
    }
    echo json_encode(['advisories' => $answer ?: new stdClass()]);
} else {
    http_response_code(404);
}
