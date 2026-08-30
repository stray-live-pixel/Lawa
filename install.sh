#!/bin/sh

# Этот файл публикуется как asset каждого GitHub Release. Workflow заменяет dev
# на точный тег до вычисления SHA-256; поэтому latest-install не делает отдельный
# сетевой запрос только ради версии и всегда выбирает архив своего release.
release_version='dev'
repository='stray-live-pixel/Lawa'

say() { printf '%s\n' "$*"; }
warn() { printf 'ПРЕДУПРЕЖДЕНИЕ: %s\n' "$*" >&2; }
die() {
	printf 'ОШИБКА: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'USAGE'
Установка Lawa для macOS и Linux.

Использование:
  install.sh [--version vMAJOR.MINOR.PATCH] [--install-dir <путь>]
             [--codex-home <путь>] [--plan] [--yes]
             [--install-plantuml]

Параметры:
  --version               Release; по умолчанию версия самого install.sh.
  --install-dir           Каталог бинарника; по умолчанию ~/.local/bin.
  --codex-home            Корень Codex; по умолчанию $CODEX_HOME или ~/.codex.
  --plan                  Только показать проверки и изменения.
  --yes                   Подтвердить файлы Lawa и PATH без /dev/tty.
  --install-plantuml      Отдельно разрешить системную установку PlantUML;
                          в неинтерактивном режиме требует --yes.
  -h, --help              Показать эту справку.

Переменные окружения: LAWA_VERSION, LAWA_INSTALL_DIR, LAWA_CODEX_HOME и CODEX_HOME.
Обычный --yes никогда не разрешает пакетный менеджер или sudo.
USAGE
}

requested_version=${LAWA_VERSION:-}
install_dir=${LAWA_INSTALL_DIR:-}
codex_home=${LAWA_CODEX_HOME:-${CODEX_HOME:-}}
plan_only=0
assume_yes=0
install_plantuml_flag=0

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			[ "$#" -ge 2 ] || die "параметру --version нужно значение"
			requested_version=$2
			shift 2
			;;
		--version=*) requested_version=${1#*=}; shift ;;
		--install-dir)
			[ "$#" -ge 2 ] || die "параметру --install-dir нужен путь"
			install_dir=$2
			shift 2
			;;
		--install-dir=*) install_dir=${1#*=}; shift ;;
		--codex-home)
			[ "$#" -ge 2 ] || die "параметру --codex-home нужен путь"
			codex_home=$2
			shift 2
			;;
		--codex-home=*) codex_home=${1#*=}; shift ;;
		--plan) plan_only=1; shift ;;
		--yes) assume_yes=1; shift ;;
		--install-plantuml) install_plantuml_flag=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "неизвестный параметр $1; см. --help" ;;
	esac
done

if [ "$install_plantuml_flag" -eq 1 ] && [ "$assume_yes" -ne 1 ]; then
	die "--install-plantuml требует --yes: системные пакеты нельзя менять по обычному интерактивному подтверждению Lawa"
fi

[ -n "${HOME:-}" ] || die "HOME не задан; укажите --install-dir и --codex-home после настройки домашней папки"
[ -n "$install_dir" ] || install_dir=$HOME/.local/bin
[ -n "$codex_home" ] || codex_home=$HOME/.codex
[ -n "$requested_version" ] || requested_version=$release_version

# Абсолютные пути исключают зависимость от cwd между планом и записью. Пробелы и
# shell-метасимволы остаются обычными символами: все обращения ниже заключены в
# кавычки и не передаются eval. Двоеточие неоднозначно внутри PATH, а одинарная
# кавычка не представима в добавляемой строке без исполняемой интерполяции.
for checked_path in "$HOME" "$install_dir" "$codex_home"; do
	case "$checked_path" in
		/*) ;;
		*) die "нужен абсолютный путь: $checked_path" ;;
	esac
	case "$checked_path" in
		*'
'*) die "путь не должен содержать перевод строки" ;;
	esac
	if printf '%s' "$checked_path" | LC_ALL=C grep -q '[[:cntrl:]]'; then
		die "путь не должен содержать управляющие символы: $checked_path"
	fi
done
case "$install_dir" in
	*:*) die "каталог бинарника не должен содержать двоеточие: оно разделяет элементы PATH" ;;
	*"'"*) die "каталог бинарника с одинарной кавычкой нельзя безопасно записать в shell profile; выберите другой --install-dir" ;;
esac

if ! printf '%s\n' "$requested_version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
	die "версия должна иметь стабильный формат vMAJOR.MINOR.PATCH, получено: $requested_version"
fi

say "[1/9] Определяю платформу."
system_name=$(uname -s 2>/dev/null) || die "не удалось выполнить uname -s"
machine_name=$(uname -m 2>/dev/null) || die "не удалось выполнить uname -m"
case "$system_name" in
	Darwin) release_os=darwin ;;
	Linux) release_os=linux ;;
	*) die "система $system_name не поддерживается; нужны macOS или Linux" ;;
esac
case "$machine_name" in
	x86_64|amd64) release_arch=amd64 ;;
	arm64|aarch64) release_arch=arm64 ;;
	*) die "архитектура $machine_name не поддерживается; нужны amd64 или arm64" ;;
esac

if command -v curl >/dev/null 2>&1; then
	downloader=curl
elif command -v wget >/dev/null 2>&1; then
	downloader=wget
else
	die "не найдены curl и wget; установите один из них и повторите запуск"
fi
if command -v sha256sum >/dev/null 2>&1; then
	checksum_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
	checksum_tool=shasum
else
	die "не найдены sha256sum и shasum; установите средство SHA-256 и повторите запуск"
fi

archive_name=lawa_${requested_version}_${release_os}_${release_arch}.tar.gz
release_root=https://github.com/$repository/releases/download/$requested_version
binary_path=$install_dir/lawa
skill_dir=$codex_home/skills/lawa
skill_path=$skill_dir/SKILL.md

# Конфликты целевых типов проверяются до отдельного разрешения PlantUML: системный
# пакет не должен устанавливаться, если Lawa заведомо некуда записать.
for target_directory in "$install_dir" "$codex_home" "$codex_home/skills" "$skill_dir"; do
	if { [ -e "$target_directory" ] || [ -L "$target_directory" ]; } && [ ! -d "$target_directory" ]; then
		die "ожидался каталог, но существует другой тип файла: $target_directory"
	fi
done
if [ -e "$binary_path" ] && [ ! -f "$binary_path" ] && [ ! -L "$binary_path" ]; then
	die "целевой путь бинарника имеет неподдерживаемый тип: $binary_path"
fi
if [ -e "$skill_path" ] && [ ! -f "$skill_path" ] && [ ! -L "$skill_path" ]; then
	die "целевой путь скилла имеет неподдерживаемый тип: $skill_path"
fi

say "[2/9] Проверяю текущую установку."
if [ -x "$binary_path" ]; then
	if current_version=$("$binary_path" version 2>/dev/null); then
		current_installation=$current_version
	else
		current_installation="файл существует, но команда version завершилась ошибкой"
	fi
elif [ -e "$binary_path" ]; then
	current_installation="файл существует, но не является исполняемым"
else
	current_installation="Lawa ещё не установлена"
fi

say "[3/9] Проверяю PlantUML."
plantuml_available=0
plantuml_path='не найден'
plantuml_version='неизвестна'
if plantuml_path_candidate=$(command -v plantuml 2>/dev/null); then
	if plantuml_output=$(plantuml -version 2>&1); then
		plantuml_available=1
		plantuml_path=$plantuml_path_candidate
		plantuml_version=$(printf '%s\n' "$plantuml_output" | sed -n '1p')
	else
		plantuml_path=$plantuml_path_candidate
		plantuml_version='команда найдена, но завершается ошибкой'
	fi
fi

plantuml_manager=''
plantuml_command='ручная установка: https://plantuml.com/command-line'
use_sudo=0
if [ "$plantuml_available" -ne 1 ]; then
	if [ "$release_os" = darwin ] && command -v brew >/dev/null 2>&1; then
		plantuml_manager=brew
		plantuml_command='brew install plantuml'
	elif [ "$release_os" = linux ]; then
		if [ "$(id -u 2>/dev/null)" != 0 ]; then
			use_sudo=1
		fi
		if command -v apt-get >/dev/null 2>&1; then
			plantuml_manager=apt
			if [ "$use_sudo" -eq 1 ]; then
				plantuml_command='sudo apt-get update && sudo apt-get install -y plantuml'
			else
				plantuml_command='apt-get update && apt-get install -y plantuml'
			fi
		elif command -v dnf >/dev/null 2>&1; then
			plantuml_manager=dnf
			if [ "$use_sudo" -eq 1 ]; then
				plantuml_command='sudo dnf install -y plantuml'
			else
				plantuml_command='dnf install -y plantuml'
			fi
		elif command -v pacman >/dev/null 2>&1; then
			plantuml_manager=pacman
			if [ "$use_sudo" -eq 1 ]; then
				plantuml_command='sudo pacman -S --needed --noconfirm plantuml'
			else
				plantuml_command='pacman -S --needed --noconfirm plantuml'
			fi
		fi
	fi
fi

path_change=0
path_missing=0
profile_path='не изменяется'
profile_display=$profile_path
path_line="export PATH='$install_dir':\"\$PATH\""
case ":${PATH:-}:" in
	*":$install_dir:"*) ;;
	*)
		path_missing=1
		case "${SHELL:-}" in
			*/zsh) profile_path=$HOME/.zshrc ;;
			*/bash) profile_path=$HOME/.bashrc ;;
			*) profile_path=$HOME/.profile ;;
		esac
		profile_display=$profile_path
		profile_links=0
		while [ -L "$profile_path" ]; do
			command -v readlink >/dev/null 2>&1 || die "shell profile $profile_path является символьной ссылкой, но readlink недоступен"
			profile_target=$(readlink "$profile_path") || die "не удалось прочитать символьную ссылку shell profile $profile_path"
			case "$profile_target" in
				/*) profile_path=$profile_target ;;
				*) profile_path=$(dirname "$profile_path")/$profile_target ;;
			esac
			profile_links=$((profile_links + 1))
			[ "$profile_links" -le 20 ] || die "слишком длинная цепочка символьных ссылок shell profile"
		done
		if printf '%s' "$profile_path" | LC_ALL=C grep -q '[[:cntrl:]]'; then
			die "путь shell profile не должен содержать управляющие символы"
		fi
		if [ "$profile_display" != "$profile_path" ]; then
			profile_display="$profile_display → $profile_path"
		fi
		if [ -e "$profile_path" ] && [ ! -f "$profile_path" ]; then
			die "shell profile имеет неподдерживаемый тип: $profile_path"
		fi
		if [ -f "$profile_path" ] && grep -Fqx "$path_line" "$profile_path"; then
			profile_display="$profile_display (строка уже добавлена; нужна новая shell-сессия)"
		else
			path_change=1
		fi
		;;
esac

say "[4/9] План установки."
say "  Версия: $requested_version"
say "  Платформа: $release_os/$release_arch"
say "  Текущая установка: $current_installation"
say "  Новый бинарник: $binary_path"
say "  Новый скилл: $skill_path"
if [ "$plantuml_available" -eq 1 ]; then
	say "  PlantUML: $plantuml_path — $plantuml_version"
else
	warn "PlantUML недоступен ($plantuml_path: $plantuml_version). Lawa и текстовые статусы будут работать, но PNG-схемы создаваться не будут."
	say "  Команда установки PlantUML: $plantuml_command"
	if [ "$use_sudo" -eq 1 ]; then
		say "  ВАЖНО: команда запросит sudo и получит право изменить системные пакеты."
	fi
fi
if [ "$path_change" -eq 1 ]; then
	say "  Shell profile: $profile_display"
	say "  Добавляемая строка: $path_line"
else
	say "  Shell profile: $profile_display"
fi
say "  Архив: $release_root/$archive_name"
say "  Проверка: $checksum_tool и SHA256SUMS"

if [ "$plan_only" -eq 1 ]; then
	say "Режим --plan: файлы, PATH и системные пакеты не изменены."
	exit 0
fi

# В production ответы читаются только с терминала, поэтому `curl ... | sh` не
# поглощает stdin установщика. Тестовый файл разрешён исключительно вместе с
# LAWA_TESTING=1 и не является пользовательским способом обхода подтверждения.
tty_opened=0
open_tty() {
	[ "$tty_opened" -eq 0 ] || return 0
	if [ "${LAWA_TESTING:-}" = 1 ] && [ -n "${LAWA_TEST_TTY:-}" ]; then
		exec 3< "$LAWA_TEST_TTY" || return 1
		tty_output=2
	else
		exec 3<> /dev/tty || return 1
		tty_output=3
	fi
	tty_opened=1
}

prompt_yes() {
	prompt_text=$1
	open_tty || die "нужен интерактивный терминал /dev/tty; для проверенной автоматизации используйте --yes"
	if [ "$tty_output" -eq 3 ]; then
		printf '%s [y/N]: ' "$prompt_text" >&3
	else
		printf '%s [y/N]: ' "$prompt_text" >&2
	fi
	IFS= read -r prompt_answer <&3 || prompt_answer=''
	case "$prompt_answer" in
		y|Y|yes|YES|Yes|да|Да|ДА) return 0 ;;
		*) return 1 ;;
	esac
}

if [ "$assume_yes" -ne 1 ]; then
	if ! prompt_yes "Установить Lawa $requested_version в показанные пути?"; then
		say "Установка отменена. Файлы и системные пакеты не изменены."
		exit 0
	fi
fi

install_plantuml_now=0
if [ "$plantuml_available" -ne 1 ]; then
	if [ "$install_plantuml_flag" -eq 1 ]; then
		install_plantuml_now=1
	elif [ "$assume_yes" -ne 1 ]; then
		if prompt_yes "Отдельно установить PlantUML командой: $plantuml_command ?"; then
			install_plantuml_now=1
		fi
	fi
fi

run_with_privilege() {
	if [ "$use_sudo" -eq 1 ]; then
		command -v sudo >/dev/null 2>&1 || die "для установки PlantUML нужен sudo, но команда sudo не найдена"
		sudo "$@"
	else
		"$@"
	fi
}

if [ "$install_plantuml_now" -eq 1 ]; then
	[ -n "$plantuml_manager" ] || die "неизвестен безопасный пакетный менеджер для PlantUML; выполните вручную: https://plantuml.com/command-line"
	say "[5/9] Устанавливаю PlantUML отдельно разрешённой командой: $plantuml_command"
	case "$plantuml_manager" in
		brew)
			brew install plantuml || die "Homebrew не установил PlantUML; файлы Lawa ещё не изменялись"
			;;
		apt)
			run_with_privilege apt-get update || die "apt-get update завершился ошибкой; файлы Lawa ещё не изменялись, но пакетный менеджер мог изменить своё состояние"
			run_with_privilege apt-get install -y plantuml || die "apt-get не установил PlantUML; файлы Lawa ещё не изменялись, но системные пакеты могли частично измениться"
			;;
		dnf)
			run_with_privilege dnf install -y plantuml || die "dnf не установил PlantUML; файлы Lawa ещё не изменялись, но системные пакеты могли частично измениться"
			;;
		pacman)
			run_with_privilege pacman -S --needed --noconfirm plantuml || die "pacman не установил PlantUML; файлы Lawa ещё не изменялись, но системные пакеты могли частично измениться"
			;;
	esac
	if ! command -v plantuml >/dev/null 2>&1 || ! plantuml_output=$(plantuml -version 2>&1); then
		die "пакетный менеджер завершился успешно, но plantuml -version не работает; файлы Lawa ещё не изменялись"
	fi
	plantuml_available=1
	say "PlantUML проверен: $(printf '%s\n' "$plantuml_output" | sed -n '1p')"
else
	say "[5/9] Системные пакеты не изменяю."
fi

temporary_dir=''
stage_binary=''
stage_skill=''
stage_profile=''
backup_binary=$binary_path.lawa-backup.$$
backup_skill=$skill_path.lawa-backup.$$
backup_profile=''
transaction_active=0
binary_had_old=0
skill_had_old=0
profile_had_old=0
binary_new=0
skill_new=0
profile_new=0

rollback_installation() {
	warn "Откатываю файлы Lawa к состоянию до запуска."
	if [ "$profile_new" -eq 1 ]; then rm -f "$profile_path" || warn "не удалось удалить новый profile $profile_path"; fi
	if [ "$profile_had_old" -eq 1 ] && [ -e "$backup_profile" ]; then mv "$backup_profile" "$profile_path" || warn "не удалось восстановить $profile_path из $backup_profile"; fi
	if [ "$skill_new" -eq 1 ]; then rm -f "$skill_path" || warn "не удалось удалить новый скилл $skill_path"; fi
	if [ "$skill_had_old" -eq 1 ] && [ -e "$backup_skill" ]; then mv "$backup_skill" "$skill_path" || warn "не удалось восстановить $skill_path из $backup_skill"; fi
	if [ "$binary_new" -eq 1 ]; then rm -f "$binary_path" || warn "не удалось удалить новый бинарник $binary_path"; fi
	if [ "$binary_had_old" -eq 1 ] && [ -e "$backup_binary" ]; then mv "$backup_binary" "$binary_path" || warn "не удалось восстановить $binary_path из $backup_binary"; fi
	transaction_active=0
}

on_exit() {
	exit_status=$1
	trap - 0
	if [ "$transaction_active" -eq 1 ]; then rollback_installation; fi
	[ -n "$stage_binary" ] && rm -f "$stage_binary"
	[ -n "$stage_skill" ] && rm -f "$stage_skill"
	[ -n "$stage_profile" ] && rm -f "$stage_profile"
	[ -n "$temporary_dir" ] && rm -rf "$temporary_dir"
	exit "$exit_status"
}
trap 'on_exit $?' 0
trap 'exit 129' 1
trap 'exit 130' 2
trap 'exit 143' 15

download_to() {
	download_url=$1
	download_target=$2
	if [ "$downloader" = curl ]; then
		curl -fL --retry 3 --connect-timeout 10 -o "$download_target" "$download_url"
	else
		wget -q -O "$download_target" "$download_url"
	fi
}

say "[6/9] Загружаю release во временную папку."
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/lawa-install.XXXXXX") || die "не удалось создать временную папку"
checksum_path=$temporary_dir/SHA256SUMS
archive_path=$temporary_dir/$archive_name
download_to "$release_root/SHA256SUMS" "$checksum_path" || die "не удалось скачать $release_root/SHA256SUMS; установленная Lawa не изменена"
download_to "$release_root/$archive_name" "$archive_path" || die "не удалось скачать $release_root/$archive_name; установленная Lawa не изменена"

say "[7/9] Проверяю SHA-256 до распаковки и записи пользовательских файлов."
expected_checksum=$(awk -v filename="$archive_name" '$2 == filename || $2 == "*" filename { print $1; exit }' "$checksum_path")
if ! printf '%s\n' "$expected_checksum" | grep -Eq '^[0-9a-fA-F]{64}$'; then
	die "SHA256SUMS не содержит корректный checksum для $archive_name; установленная Lawa не изменена"
fi
if [ "$checksum_tool" = sha256sum ]; then
	actual_checksum=$(sha256sum "$archive_path" | awk '{print $1}') || die "sha256sum не смог проверить $archive_path"
else
	actual_checksum=$(shasum -a 256 "$archive_path" | awk '{print $1}') || die "shasum не смог проверить $archive_path"
fi
if [ "$expected_checksum" != "$actual_checksum" ]; then
	die "checksum архива не совпал; ожидался $expected_checksum, получен $actual_checksum. Установленная Lawa не изменена"
fi

unpack_dir=$temporary_dir/unpack
mkdir "$unpack_dir" || die "не удалось создать папку распаковки $unpack_dir"
tar -xzf "$archive_path" -C "$unpack_dir" || die "не удалось распаковать проверенный архив $archive_path"
[ -f "$unpack_dir/lawa" ] || die "архив $archive_name не содержит бинарник lawa"

say "[8/9] Готовлю бинарник, скилл и PATH до атомарной замены."
mkdir -p "$install_dir" || die "не удалось создать каталог бинарника $install_dir"
mkdir -p "$skill_dir" || die "не удалось создать каталог скилла $skill_dir"
[ ! -d "$binary_path" ] || die "целевой путь бинарника является каталогом: $binary_path"
[ ! -d "$skill_path" ] || die "целевой путь скилла является каталогом: $skill_path"

stage_binary=$(mktemp "$install_dir/.lawa-new.XXXXXX") || die "не удалось создать временный бинарник в $install_dir"
cp "$unpack_dir/lawa" "$stage_binary" || die "не удалось подготовить бинарник в $install_dir"
chmod 0755 "$stage_binary" || die "не удалось сделать подготовленный бинарник исполняемым"
staged_version=$("$stage_binary" version 2>/dev/null) || die "загруженный бинарник не выполняет lawa version"
[ "$staged_version" = "$requested_version" ] || die "версия загруженного бинарника $staged_version не совпадает с release $requested_version"

stage_skill=$(mktemp "$skill_dir/.SKILL.md-new.XXXXXX") || die "не удалось создать временный скилл в $skill_dir"
"$stage_binary" skill > "$stage_skill" || die "бинарник $requested_version не смог сгенерировать SKILL.md"
chmod 0644 "$stage_skill" || die "не удалось установить права подготовленного SKILL.md"
[ -s "$stage_skill" ] || die "бинарник $requested_version вернул пустой SKILL.md"

if [ "$path_change" -eq 1 ]; then
	profile_parent=$(dirname "$profile_path")
	mkdir -p "$profile_parent" || die "не удалось создать каталог shell profile $profile_parent"
	stage_profile=$(mktemp "$profile_parent/.lawa-profile-new.XXXXXX") || die "не удалось подготовить shell profile рядом с $profile_path"
	if [ -f "$profile_path" ]; then
		cp -p "$profile_path" "$stage_profile" || die "не удалось скопировать $profile_path для безопасного изменения"
	fi
	printf '\n# Добавлено установщиком Lawa; повторный запуск не дублирует строку.\n%s\n' "$path_line" >> "$stage_profile" || die "не удалось подготовить изменение PATH для $profile_path"
	backup_profile=$profile_path.lawa-backup.$$
fi

for backup_path in "$backup_binary" "$backup_skill"; do
	[ ! -e "$backup_path" ] || die "временный backup уже существует: $backup_path"
done
if [ -n "$backup_profile" ]; then [ ! -e "$backup_profile" ] || die "временный backup уже существует: $backup_profile"; fi

# С этого места любое завершение запускает rollback из trap. Старые файлы
# перемещаются, а не удаляются; новые уже полностью проверены и находятся на тех
# же файловых системах, поэтому финальные mv атомарны для каждого пути.
transaction_active=1
if [ -e "$binary_path" ] || [ -L "$binary_path" ]; then
	binary_had_old=1
	mv "$binary_path" "$backup_binary" || die "не удалось сохранить прежний бинарник $binary_path"
fi
if [ -e "$skill_path" ] || [ -L "$skill_path" ]; then
	skill_had_old=1
	mv "$skill_path" "$backup_skill" || die "не удалось сохранить прежний скилл $skill_path"
fi
if [ -n "$backup_profile" ] && { [ -e "$profile_path" ] || [ -L "$profile_path" ]; }; then
	profile_had_old=1
	mv "$profile_path" "$backup_profile" || die "не удалось сохранить прежний shell profile $profile_path"
fi

binary_new=1
mv "$stage_binary" "$binary_path" || die "не удалось опубликовать новый бинарник $binary_path"
stage_binary=''
skill_new=1
mv "$stage_skill" "$skill_path" || die "не удалось опубликовать новый скилл $skill_path"
stage_skill=''
if [ -n "$stage_profile" ]; then
	profile_new=1
	mv "$stage_profile" "$profile_path" || die "не удалось опубликовать новый shell profile $profile_path"
	stage_profile=''
fi

if ! final_version=$("$binary_path" version 2>/dev/null) || [ "$final_version" != "$requested_version" ]; then
	die "финальная проверка $binary_path version не подтвердила $requested_version"
fi
transaction_active=0
rm -f "$backup_binary" "$backup_skill"
[ -n "$backup_profile" ] && rm -f "$backup_profile"

say "[9/9] Lawa $requested_version установлена и проверена."
say "  Бинарник: $binary_path"
say "  Скилл: $skill_path"
if [ "$path_missing" -eq 1 ]; then
	if [ "$path_change" -eq 1 ]; then
		say "  PATH обновлён в: $profile_display"
	else
		say "  PATH уже настроен в profile, но не обновлён в текущей сессии."
	fi
	say "  Для текущей сессии выполните: $path_line"
fi
say "Перезапустите Codex или обновите список скиллов."
if [ "$plantuml_available" -ne 1 ]; then
	warn "PlantUML не установлен. Workflow и текстовые статусы работают, PNG-схемы недоступны. Команда: $plantuml_command"
fi
