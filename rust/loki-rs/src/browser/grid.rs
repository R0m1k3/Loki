//! Grille de coordonnées sur la capture envoyée au modèle — port de
//! `browser_grid.go`. Lignes graduées tous les 100 px et étiquettes en
//! police bitmap 3×5 : un petit modèle y lit x/y pour `browser_click_xy`.
//! Mêmes pixels que le Go (l'encodeur PNG, lui, diffère).

use image::{ImageFormat, Rgba, RgbaImage};

/// `gridStep`.
const GRID_STEP: u32 = 100;

/// `digits5x3`.
fn glyph(c: char) -> Option<[&'static str; 5]> {
    Some(match c {
        '0' => ["###", "# #", "# #", "# #", "###"],
        '1' => [" # ", "## ", " # ", " # ", "###"],
        '2' => ["###", "  #", "###", "#  ", "###"],
        '3' => ["###", "  #", "###", "  #", "###"],
        '4' => ["# #", "# #", "###", "  #", "  #"],
        '5' => ["###", "#  ", "###", "  #", "###"],
        '6' => ["###", "#  ", "###", "# #", "###"],
        '7' => ["###", "  #", "  #", "  #", "  #"],
        '8' => ["###", "# #", "###", "# #", "###"],
        '9' => ["###", "# #", "###", "  #", "###"],
        _ => return None,
    })
}

const LINE: [u8; 4] = [255, 0, 200, 130];
const LABEL_BG: [u8; 4] = [0, 0, 0, 200];
const LABEL_FG: [u8; 4] = [255, 255, 0, 255];

/// `overlayGrid` : le PNG avec sa grille ; illisible, rendu tel quel.
pub fn overlay_grid(raw: &[u8]) -> Vec<u8> {
    let Ok(src) = image::load_from_memory_with_format(raw, ImageFormat::Png) else {
        return raw.to_vec();
    };
    let mut img = src.to_rgba8();
    draw(&mut img);
    let mut out = std::io::Cursor::new(Vec::new());
    if img.write_to(&mut out, ImageFormat::Png).is_err() {
        return raw.to_vec();
    }
    out.into_inner()
}

/// La grille et ses graduations, dessinées sur l'image.
pub fn draw(img: &mut RgbaImage) {
    let (w, h) = img.dimensions();
    let mut x = GRID_STEP;
    while x < w {
        for y in 0..h {
            blend(img, x as i64, y as i64, LINE);
        }
        draw_label(img, x as i64 + 2, 2, &x.to_string());
        x += GRID_STEP;
    }
    let mut y = GRID_STEP;
    while y < h {
        for x in 0..w {
            blend(img, x as i64, y as i64, LINE);
        }
        draw_label(img, 2, y as i64 + 2, &y.to_string());
        y += GRID_STEP;
    }
}

fn inside(img: &RgbaImage, x: i64, y: i64) -> bool {
    x >= 0 && y >= 0 && (x as u32) < img.width() && (y as u32) < img.height()
}

/// `blend` : couleur translucide sur un pixel (opaque ensuite).
fn blend(img: &mut RgbaImage, x: i64, y: i64, c: [u8; 4]) {
    if !inside(img, x, y) {
        return;
    }
    let a = f64::from(c[3]) / 255.0;
    let o = img.get_pixel(x as u32, y as u32).0;
    let mix = |n: u8, o: u8| (f64::from(n) * a + f64::from(o) * (1.0 - a)) as u8;
    img.put_pixel(
        x as u32,
        y as u32,
        Rgba([mix(c[0], o[0]), mix(c[1], o[1]), mix(c[2], o[2]), 255]),
    );
}

fn set_opaque(img: &mut RgbaImage, x: i64, y: i64, c: [u8; 4]) {
    if inside(img, x, y) {
        img.put_pixel(x as u32, y as u32, Rgba([c[0], c[1], c[2], 255]));
    }
}

/// `drawLabel` : un nombre en police 3×5 (×2) sur fond sombre.
fn draw_label(img: &mut RgbaImage, x: i64, y: i64, s: &str) {
    const SC: i64 = 2;
    let (glyph_w, glyph_h, gap) = (3 * SC, 5 * SC, SC);
    let box_w = s.chars().count() as i64 * (glyph_w + gap) + gap;
    let box_h = glyph_h + 2 * gap;
    for dy in -gap..box_h - gap {
        for dx in -gap..box_w - gap {
            set_opaque(img, x + dx, y + dy, LABEL_BG);
        }
    }
    let mut cx = x;
    for c in s.chars() {
        if let Some(g) = glyph(c) {
            for (row, line) in g.iter().enumerate() {
                for (col, b) in line.bytes().enumerate() {
                    if b != b'#' {
                        continue;
                    }
                    for iy in 0..SC {
                        for ix in 0..SC {
                            set_opaque(
                                img,
                                cx + col as i64 * SC + ix,
                                y + row as i64 * SC + iy,
                                LABEL_FG,
                            );
                        }
                    }
                }
            }
        }
        cx += glyph_w + gap;
    }
}
