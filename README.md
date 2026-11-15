# Distro

Messagerie communautaire temps réel : serveurs, salons textuels **et vocaux**, messages privés, **appels vocaux privés**, rôles, réactions, réponses,
**images, MP3 et fichiers joints (ZIP…)**, indicateur de frappe, présence en ligne. Thème noir / bleu nuit.

Un seul binaire Go, **aucune dépendance** (bibliothèque standard uniquement), front intégré.

## Lancer

    go run .                      # http://localhost:8080
    # ou un binaire précompilé du dossier bin/ :
    ./bin/distro-linux-amd64
    bin\distro-windows-amd64.exe

Options : `-addr :8080` `-data ./data`, ou variables `PORT`, `DATA_DIR`.

| Variable          | Rôle                                                              |
|-------------------|-------------------------------------------------------------------|
| `PORT`            | port d'écoute (défaut 8080)                                       |
| `DATA_DIR`        | dossier des données (défaut `data`) — **à sauvegarder**           |
| `SECURE_COOKIES=1`| cookie de session `Secure` (à activer derrière HTTPS)             |
| `TRUST_PROXY=1`   | lit `X-Forwarded-For` pour la limitation de débit (derrière proxy)|
| `MAX_UPLOAD_MB`   | taille maximale d'un fichier joint (défaut 25) — option `-max-upload` |

**Recompiler** (obligatoire après modification du code ou de `web/`, qui est embarqué dans le binaire) :
`build.bat` (Windows) ou `./build.sh`, ou simplement `go run .` — Go 1.24+ requis.

## Héberger (le plus simple)

**VPS + Caddy (HTTPS automatique)** — copie le binaire Linux, puis :

    SECURE_COOKIES=1 TRUST_PROXY=1 ./distro-linux-amd64

`Caddyfile` :

    chat.mondomaine.fr {
        reverse_proxy localhost:8080 {
            flush_interval -1
        }
    }

(`flush_interval -1` : indispensable pour que le flux temps réel ne soit pas mis en mémoire tampon.
Avec nginx : `proxy_buffering off; proxy_read_timeout 1h; client_max_body_size 30m;` — sans ce dernier réglage nginx
refuse tout envoi de plus de 1 Mo, quelle que soit la valeur de `MAX_UPLOAD_MB`.)

**Docker**

    docker build -t distro .
    docker run -d -p 8080:8080 -v distro-data:/data -e SECURE_COOKIES=1 -e TRUST_PROXY=1 distro

Tout hébergeur qui exécute un binaire ou un conteneur avec un disque persistant convient.

## Rôles

Chaque serveur a ses propres **rôles personnalisés** : un nom, une couleur et des permissions.
Menu du serveur (clic sur son nom) → **Rôles** ; clic sur un membre → **Gérer les rôles**.

| Permission  | Autorise                                              |
|-------------|-------------------------------------------------------|
| `channels`  | créer et supprimer des salons                         |
| `messages`  | supprimer les messages des autres                     |
| `kick`      | expulser des membres                                  |
| `roles`     | créer, modifier et attribuer des rôles                |
| `server`    | personnaliser le serveur (nom, icône, couleur, description) |

- **Hiérarchie** : l'ordre de la liste compte (le haut est le plus puissant, réordonnable avec ↑ ↓).
  On ne gère que les rôles et les membres situés **strictement en dessous** de son propre rôle plus haut.
  À rang égal, personne n'a autorité sur l'autre (ex. deux modérateurs ne peuvent pas s'expulser).
- **Pas d'escalade** : on ne peut donner, ou attribuer via un rôle, que des permissions qu'on possède soi-même.
  On ne peut pas se donner un rôle à soi-même.
- Le **propriétaire** a toujours tous les droits et ne peut être ni expulsé ni modifié par personne.
- Le pseudo d'un membre prend la couleur de son rôle le plus haut (liste des membres et messages).
- Limites : 25 rôles par serveur, nom de 1 à 24 caractères ; couleurs `#rrggbb`.
- **Migration** : les anciens « admin » deviennent membres d'un vrai rôle *Admin* (salons, messages,
  expulsions), automatiquement au premier démarrage. Rien à faire.

API : `POST /api/servers/{id}/roles` · `PATCH|DELETE /api/servers/{id}/roles/{rid}` ·
`POST /api/servers/{id}/roles/{rid}/move` (`{"dir":-1|1}`) · `POST /api/servers/{id}/members/{uid}/roles` (`{"role_id":…,"on":true|false}`).

## Vocal : salons vocaux et appels privés

**Salons vocaux** — dans un serveur, `+` à côté de « Salons vocaux » (ou `+` des salons, onglet *Vocal*) ; permission `channels`,
comme pour les salons textuels. Un clic sur un salon vocal le rejoint ; les personnes connectées s'affichent dessous, avec un
**anneau vert quand elles parlent** et une icône quand leur micro (🔇) ou leur son (🙉) est coupé. On ne peut être que dans **un
seul** salon vocal à la fois. Un salon vocal n'a pas de messages. Un serveur doit garder au moins un salon *textuel*.

**Appels privés** — dans un message privé, bouton **📞 Appeler** (en-tête). L'autre personne voit une carte
« Appel entrant » avec *Accepter* / *Refuser* et une sonnerie ; ça sonne 45 secondes. Raccrocher d'un côté termine l'appel.

**Barre vocale** (en bas à gauche, au-dessus de ton profil) : couper le micro 🎙️, couper le son 🎧 (coupe aussi le micro), quitter 📞.
Sans micro (refusé / absent), on rejoint quand même en **écoute seule**. Modération : *Déconnecter du vocal* dans le menu d'un
membre (permission `kick`, hiérarchie respectée).

Comment ça marche : l'audio est en **WebRTC pair à pair** (aucun flux audio ne passe par le serveur). Le serveur ne relaie que la
signalisation par le flux temps réel existant. Maillage complet : **8 personnes maximum par salon** (au-delà, la bande passante
de chacun explose ; passer à un SFU serait l'étape suivante). L'état vocal est en mémoire : un redémarrage du serveur vide les salons.

**À savoir pour l'hébergement :**

- **HTTPS obligatoire** pour le micro (sauf sur `localhost`). Derrière Caddy, c'est déjà le cas.
- **STUN** : les serveurs publics de Google sont utilisés par défaut. Pour les réseaux restrictifs (4G, entreprises, pare-feu
  stricts), ajoute un **serveur TURN** (ex. coturn) :

      TURN_URL="turn:chat.mondomaine.fr:3478,turns:chat.mondomaine.fr:5349" TURN_USER=distro TURN_PASS=secret ./distro-linux-amd64

  Les identifiants TURN sont envoyés aux utilisateurs connectés : utilise un compte dédié, sans autre droit.
- La CSP stricte du serveur n'est pas modifiée (WebRTC n'est pas soumis à `connect-src`).

API : `POST /api/voice/{id}/join` (`{muted, deaf, tab}` → `{peers, ice, users, ringing, dm}`) · `POST /api/voice/leave` ·
`POST /api/voice/{id}/state` (`{muted, deaf}`) · `POST /api/voice/{id}/signal` (`{to, kind: offer|answer|ice, data}`) ·
`POST /api/voice/{id}/decline` · `POST /api/voice/{id}/kick/{uid}`. `{id}` = ID d'un salon vocal ou d'un salon de MP.
`POST /api/servers/{id}/channels` accepte `kind: "voice"`. Évènements temps réel : `voice.state`, `voice.signal`, `voice.end`,
`call.ring`, `call.cancel`. `GET /api/me` renvoie aussi `voice` (état de tous les salons vocaux visibles).

## Sauvegarde des données

Tout est enregistré automatiquement dans `DATA_DIR` (défaut : le dossier `data` **du dossier où Distro est lancé**) :
`state.json` (comptes, serveurs, salons, rôles, profils, MP), `messages.jsonl` (messages, réactions), `media/` (avatars, icônes)
et `files/` (pièces jointes des messages).
L'état est écrit chaque seconde (et forcé sur le disque), les messages immédiatement. Au démarrage, une copie de sécurité est
faite dans `backups/` (les 10 dernières sont gardées). Le chemin exact s'affiche au lancement : **toujours lancer Distro avec le même
dossier de données** (`-data chemin`), sinon un nouveau dossier vide est créé. Pour déplacer ou sauvegarder Distro : copier tout `DATA_DIR`.

## Fichiers et images dans les messages

Trois façons de joindre : le bouton **📎** à gauche de la zone de saisie, le **glisser-déposer** sur la conversation,
ou **Ctrl+V** pour coller une capture d'écran ou une image copiée. Les fichiers choisis apparaissent au-dessus de la zone de saisie
(retirables avec ×) ; on peut les envoyer avec ou sans texte. Une barre de progression s'affiche pendant l'envoi, avec un bouton *Annuler*.

- **Limites** : 5 fichiers par message, 25 Mo chacun par défaut (`MAX_UPLOAD_MB`). Les fichiers vides sont refusés.
- **Images** (PNG, JPEG, GIF, WebP) : affichées dans le message ; un clic les ouvre en grand avec un bouton *Télécharger*.
- **Audio** (MP3, M4A, AAC, FLAC, WAV, OGG/Opus) : lecteur intégré, on peut avancer dans la piste.
- **Vidéo** (MP4, WebM) : lecteur intégré.
- **Tout le reste** (ZIP, PDF, documents, exécutables…) : carte avec nom, taille et téléchargement.
- Un fichier est **supprimé du disque avec son message** (ou avec son salon / son serveur).

Sécurité : le type d'un fichier est déterminé par son **contenu**, jamais par son nom ni par ce qu'annonce le navigateur. Seuls les types
ci-dessus sont affichés en ligne ; un `.png` qui contient en réalité du HTML, un SVG ou un `.exe` sont simplement proposés en téléchargement
(`application/octet-stream`, `Content-Disposition: attachment`, `Content-Security-Policy: sandbox`) : rien ne peut s'exécuter dans la page.
Un fichier n'est lisible que par les gens qui ont accès au salon du message (membres du serveur, ou les deux personnes d'un MP) ;
les envois sont limités en débit (8 par minute et par personne) et écrits directement sur le disque, sans passer par la mémoire.

API : `POST /api/channels/{id}/messages` en `multipart/form-data` (champs `content`, `reply_to`, et un ou plusieurs `file`) ·
`GET /api/files/{id}` (`?dl=1` force le téléchargement, gère les requêtes `Range`). Le JSON d'un message contient
`files: [{id, name, size, kind}]` avec `kind` = `image` | `audio` | `video` | `file`. `GET /api/me` renvoie aussi `limits`.

## Personnalisation

**Profil** (⚙ dans la barre en bas à gauche, ou clic sur ton avatar) : nom affiché, statut, « À propos de moi »
(190 caractères), avatar (image), couleur du profil et couleur de bannière, avec aperçu en direct.
Le **pseudo de connexion reste l'identifiant** (c'est lui qu'on tape pour t'écrire en MP) : il est toujours rappelé
(`@pseudo`) sur la carte de profil, ce qui évite l'usurpation par nom affiché. Clic sur un avatar, un nom d'auteur ou
« Voir le profil » dans la liste des membres → carte de profil (avec les rôles du membre dans le serveur).

**Serveur** (menu du serveur → *Personnaliser le serveur*, permission `server` ou propriétaire) : nom, description,
icône (image) et couleur. La couleur teinte le bouton du serveur et souligne son en-tête ; l'icône remplace les initiales.
**Salons** : le crayon ✎ au survol d'un salon (permission `channels`) change son nom et son **sujet**, affiché dans l'en-tête.

Images : recadrées en carré et réduites à 256 px dans le navigateur (WebP, sinon JPEG), 200 Ko maximum côté serveur,
PNG / JPEG / WebP uniquement (jamais de SVG). Elles sont stockées dans `DATA_DIR/media/` (**à sauvegarder avec le reste**)
et servies avec une URL versionnée, donc mises en cache par le navigateur.

API : `PATCH /api/me` (`display`, `status`, `bio`, `color`, `banner`) · `PUT|DELETE /api/me/avatar` (corps = image brute) ·
`GET /api/users/{id}/avatar` · `PATCH /api/servers/{id}` (`name`, `description`, `accent`) ·
`PUT|DELETE|GET /api/servers/{id}/icon` · `PATCH /api/channels/{id}` (`name`, `topic`).
Les changements de profil sont diffusés en temps réel (évènement `user.update`) aux gens qui partagent un serveur ou un MP.

## Architecture

- `store.go`  : état en mémoire + persistance. Un instantané `state.json` (comptes, serveurs, salons, sessions)
  et un journal `messages.jsonl` en ajout seul, compacté à chaque démarrage. Écritures atomiques.
- `hub.go`    : diffusion temps réel (Server-Sent Events) + limiteur de débit. Un client trop lent est
  déconnecté et se resynchronise tout seul.
- `handlers.go` : API JSON (comptes, serveurs, salons, messages).
- `files.go`  : pièces jointes : envoi multipart en flux, détection du type, stockage dans `files/`, téléchargement protégé.
- `profile.go` : personnalisation : profil, avatars, icônes de serveur, description, sujet de salon.
- `voice.go`  : vocal : salles, appels privés (sonnerie), relais de signalisation WebRTC, nettoyage à la déconnexion.
- `roles.go`  : rôles personnalisés : permissions, hiérarchie et endpoints. Toute vérification de droit passe par `Server.has()` / `Server.outranks()`.
- `web/`      : interface (HTML/CSS/JS sans framework, rendu DOM sûr — aucun `innerHTML`). `voice.js` : client WebRTC et interface vocale.

Sécurité : mots de passe PBKDF2-SHA256 (120 000 itérations) salés, sessions aléatoires stockées hachées,
cookie HttpOnly + SameSite, CSP stricte, limitation de débit (connexion, inscription, messages).

## Limites actuelles (honnêtes)

- Une seule instance : tout est en mémoire. C'est très rapide et tient largement des milliers d'utilisateurs
  et des centaines de milliers de messages, mais ce n'est pas « sans limite ». Pour aller bien plus loin :
  passer le stockage sur PostgreSQL/SQLite et la diffusion sur Redis pub/sub (le code est découpé pour ça).
- Les pièces jointes n'ont pas de quota par personne : surveiller l'espace disque de `DATA_DIR`. Pas de miniatures générées
  (l'image d'origine est affichée, réduite par le navigateur) ni d'antivirus : les fichiers sont téléchargés tels quels.
- Vocal : maillage pair à pair limité à 8 personnes par salon, audio seulement (pas de vidéo ni de partage d'écran), pas d'historique des appels manqués dans la conversation.
- Pas encore : vidéo, notifications push, e-mail de récupération de mot de passe,
  profil propre à chaque serveur (surnom par serveur), bannière en image, GIF animés.
