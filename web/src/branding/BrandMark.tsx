import { useBrand } from "./brand";
import { ProductMark } from "./ProductMark";

/**
 * What stands for the product wherever the app shows its own identity: the
 * head of the sidebar, and the pages outside the console.
 *
 * It is the product mark for everyone, whatever roles they hold — the groups
 * in the sidebar below it already say which parts of the product a person
 * has, so the head never needed to name a console. White-labelling is the one
 * thing that changes it: a tenant that has configured a logo or a product
 * name gets theirs here instead.
 */
export function BrandMark({ markClassName, logoClassName }: { markClassName?: string; logoClassName?: string }) {
  const brand = useBrand();
  if (brand.configured && (brand.logo_data_url || brand.product_name)) {
    return (
      <>
        {brand.logo_data_url && (
          // Decorative when the name is spelled out beside it: the heading
          // around this would otherwise be announced twice over.
          <img className={logoClassName} src={brand.logo_data_url} alt={brand.product_name ? "" : brand.name} data-testid="brand-logo" />
        )}
        {brand.product_name && <span>{brand.product_name}</span>}
      </>
    );
  }
  return <ProductMark className={markClassName} />;
}
