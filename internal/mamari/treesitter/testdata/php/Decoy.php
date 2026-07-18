<?php

class RepoAlias
{
    public function find_user(string $id): string
    {
        return "wrong-" . $id;
    }
}
