#!/bin/bash
handler() { echo func; }
command handler
echo a | xargs handler
handler
