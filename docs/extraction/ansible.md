# Ansible — by-name structure, read without rendering

Ansible's structure is by-name and literal: a playbook lists the roles it
applies, a role's tasks import other roles by name, and a role's templates
live under its own tree. All of it reads deterministically from YAML — and
none of it requires rendering a Jinja template, which the extractor never
does.

Fixture: [`ansible_sample`](../../internal/engine/testdata/repos/ansible_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a `roles/<name>/` directory | one module, keyed by that path | `module` |
| a YAML list entry carrying `hosts:` | a symbol with `ansible_kind: play`, named from `name:` or its filename | `symbol` |
| a role | a symbol with `ansible_kind: role` | `symbol` |
| `roles:` — `- nginx` or `- role: app` | `depends_on` from the play to each role the repository declares | relation |
| `import_role:` / `include_role:` with a literal `name:` | `depends_on` from one role to another | relation |
| a file under a role's `templates/` | `template_count` on that role's symbol | props |
| `{{ anything }}` | nothing — no template is ever rendered | — |

## Detection is the whole false-positive risk

Arbitrary YAML must not read as Ansible, so detection demands an unambiguous
marker at any depth: an `ansible.cfg`, or a role, which is a `roles/<name>/`
directory holding `tasks/`, `handlers/`, `meta/`, `defaults/` or `vars/`. A
directory merely named `roles` is not one: Rails views, RBAC manifests and
user-permission modules all use the name, and before v577 each of them was
detected as Ansible. The extractor also walks the repository itself rather than
consuming the engine's file list — YAML is ignore-globbed by default (the
OpenAPI self-walk precedent), so the excluded directories are re-skipped
explicitly.

## Plays and roles become symbols; listing a role is depending on it

```yaml
# site.yml
---
- name: Provision web tier
  hosts: web
  roles:
    - nginx
    - role: app
```

```
module  roles/nginx
module  roles/app
symbol  Provision web tier   site.yml   props: ansible_kind=play
        relations: depends_on -> roles/app.app, depends_on -> roles/nginx.nginx
symbol  roles/nginx.nginx    roles/nginx   props: ansible_kind=role, template_count=1
symbol  roles/app.app        roles/app     props: ansible_kind=role
        relations: depends_on -> roles/nginx.nginx
```

A play requires `hosts:` — a YAML list without one is data, not a play — and
takes its name from `name:` or, unnamed, from its filename. Both the string
form (`- nginx`) and the mapping form (`- role: app`) of a play's `roles:`
list resolve, and only against roles the repository actually declares
(a `roles/<name>/` directory).

## Role-to-role references are the literal names in task files

`roles/app/tasks/main.yml`:

```yaml
---
- name: base config
  import_role:
    name: nginx
```

That literal `name:` under `import_role`/`include_role` draws
`roles/app.app depends_on roles/nginx.nginx` — visible in the facts above.
A referenced name with no matching `roles/` directory draws nothing.

## Templates are counted, never rendered

`roles/nginx/templates/site.conf.j2` contributes `template_count=1` on the
nginx role's symbol. The count says a role carries templated configuration;
what the template expands to depends on inventory variables at run time, and
enola does not model runtime.

## What is deliberately not extracted

- **Jinja evaluation** — no template is ever rendered; `{{ var }}` stays
  opaque.
- **Dynamic includes** — `include_role: name: "{{ role_var }}"` names
  nothing literal and draws nothing.
- **Inventory and hosts** — which machines `hosts: web` resolves to is
  deployment state, not repository structure.
- **Galaxy dependencies** — a role required from outside the repository is
  not in the repository; nothing is fetched to model it.
