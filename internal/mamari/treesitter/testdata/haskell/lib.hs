module HsUserRepo where

import qualified Helper as H

findUserHs :: Int -> String
findUserHs id = helperHs id

helperHs :: Int -> String
helperHs x = "x"

multiArgHs :: Int -> Int -> Int
multiArgHs a b = addThreeHs a b 1

addThreeHs :: Int -> Int -> Int -> Int
addThreeHs a b c = a + b + c

qualifiedUserHs :: Int -> String
qualifiedUserHs id = H.qualifiedHelperHs id
