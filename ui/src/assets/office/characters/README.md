# Внешность сотрудников

40 образов: 30 людей и по два кота, пса, попугая, медведя и оленя.
Созданы встроенным `image_gen`, референс композиции — `../developer.png`.
Каждый PNG — отдельный персонаж со столом. Чат показывает лицо из того же спрайта,
чтобы внешность оставалась согласованной. Профессия в каталоге — сюжет образа,
а не ограничение роли агента.

## Промпты

В шаблоне ниже `CHARACTER` заменяется полем `description` из `catalog.json`.

Use case: stylized-concept. Create ONE unique office employee game sprite, using the attached reference ONLY for consistent clay style, elevated isometric camera, desk shape, composition and scale. Subject: CHARACTER. A full-body handcrafted plasticine character seated on a cream office chair at a small light-oak desk with slender dark metal legs, typing on an open silver laptop, ceramic mug. Match reference orientation: character behind desk on image left, facing diagonally toward lower right, laptop on image right, face clearly visible in three-quarter view near x44%, y20%. Head and upper torso occupy top half, feet and all desk legs visible. Square 1024px PNG, whole character and desk centered within canvas, modest margin, no cropping. Warm pastel tones, tactile clay texture, soft studio light, expressive memorable friendly face. Use real transparent alpha background, no room, no floor rectangle, no solid backdrop, no halo, no text, no watermark, no additional characters. Keep identical desk size and camera across this collection. The requested profession is visual inspiration only.
