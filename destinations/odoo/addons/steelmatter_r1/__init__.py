import secrets

from . import models


def post_init_hook(env):
    # The single journal head row. Every create, fence or refusal locks it, so
    # journal sequence order equals commit order.
    env.cr.execute("SELECT count(*) FROM steelmatter_head")
    if env.cr.fetchone()[0] == 0:
        env.cr.execute(
            "INSERT INTO steelmatter_head (last_seq, incarnation, create_uid, write_uid, create_date, write_date)"
            " VALUES (0, %s, 1, 1, now() at time zone 'UTC', now() at time zone 'UTC')",
            [secrets.token_hex(8)],
        )
