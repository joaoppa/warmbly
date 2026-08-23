#!/usr/bin/env bash
# Reproduz o que o worker faz: mantem UMA conexao aberta, seleciona o INBOX,
# e consulta o LIST-STATUS antes e depois de a mensagem chegar.
read -rp "mailbox (ex: suporte@altius.net.br): " MB
read -rsp "senha: " P; echo
L='LIST "" "%" RETURN (STATUS (MESSAGES UIDNEXT HIGHESTMODSEQ))'
{
  printf 'a1 LOGIN "%s" "%s"\n' "$MB" "$P"; sleep 3
  printf 'a2 %s\n' "$L";                    sleep 2
  printf 'a3 EXAMINE INBOX (CONDSTORE)\n';  sleep 2
  printf 'a4 %s\n' "$L";                    sleep 2
  echo ">>> ENVIE O E-MAIL AGORA. Aguardando 90s..." >&2
  sleep 90
  printf 'a5 %s\n' "$L";                    sleep 3
  printf 'a6 UNSELECT\n';                   sleep 2
  printf 'a7 %s\n' "$L";                    sleep 3
  printf 'a8 LOGOUT\n';                     sleep 2
} | openssl s_client -connect imap.hostinger.com:993 -crlf -quiet 2>/dev/null \
  | grep -iE '^\* STATUS|^a[0-9] (OK|NO|BAD)|^\* [0-9]+ (EXISTS|RECENT)'
