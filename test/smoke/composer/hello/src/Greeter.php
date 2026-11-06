<?php

namespace Smoke\Hello;

use Composer\InstalledVersions;
use Psr\Log\LoggerInterface;
use Psr\Log\NullLogger;

final class Greeter
{
    public static function greet(): string
    {
        $seven = new NullLogger() instanceof LoggerInterface ? 7 : 0;

        return "hello $seven from " . InstalledVersions::getPrettyVersion('smoke/hello');
    }
}
