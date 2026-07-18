<?php

use App\Repos\UserRepo as RepoAlias;

class UserService extends BaseService implements Greeter
{
    private RepoAlias $repo;

    public function __construct(RepoAlias $repo)
    {
        $this->repo = $repo;
    }

    public function load(string $id): string
    {
        return $this->repo->find_user($id);
    }

    public static function factory(): self
    {
        return new self(new RepoAlias());
    }
}

interface Greeter
{
    public function greet(): string;
}

function helper(): string
{
    return "hi";
}

function top_level(): string
{
    return helper();
}
