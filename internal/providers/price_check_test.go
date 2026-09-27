package providers

import "testing"

func TestCheckOfferPriceFailsClosed(t *testing.T) {
	cases := []struct {
		price, max float64
		ok         bool
	}{
		{0.5, 1, true},
		{1.04, 1, true}, // dentro da tolerância de 5%
		{1.2, 1, false}, // acima do teto
		{0, 1, false},   // preço não verificado com teto: recusa
		{-1, 1, false},
		{0, 0, true}, // sem teto pedido: não bloqueia
	}
	for _, c := range cases {
		if err := CheckOfferPrice(c.price, c.max); (err == nil) != c.ok {
			t.Errorf("CheckOfferPrice(%v, %v) = %v, quer ok=%v", c.price, c.max, err, c.ok)
		}
	}
}
